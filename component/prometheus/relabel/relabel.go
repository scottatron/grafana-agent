package relabel

import (
	"context"
	"fmt"
	"github.com/prometheus/prometheus/model/value"
	"sync"

	"go.uber.org/atomic"

	"github.com/prometheus/prometheus/storage"

	"github.com/grafana/agent/component"
	flow_relabel "github.com/grafana/agent/component/common/relabel"
	"github.com/grafana/agent/component/prometheus"
	"github.com/grafana/agent/service/labelstore"
	lru "github.com/hashicorp/golang-lru/v2"
	prometheus_client "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"

	"github.com/prometheus/prometheus/model/relabel"
)

func init() {
	component.Register(component.Registration{
		Name:    "prometheus.relabel",
		Args:    Arguments{},
		Exports: Exports{},

		Build: func(opts component.Options, args component.Arguments) (component.Component, error) {
			return New(opts, args.(Arguments))
		},
	})
}

// Arguments holds values which are used to configure the prometheus.relabel
// component.
type Arguments struct {
	// Where the relabelled metrics should be forwarded to.
	ForwardTo []labelstore.Appendable `river:"forward_to,attr"`

	// The relabelling rules to apply to each metric before it's forwarded.
	MetricRelabelConfigs []*flow_relabel.Config `river:"rule,block,optional"`

	// Cache size to use for LRU cache.
	CacheSize int `river:"max_cache_size,attr,optional"`
}

// SetToDefault implements river.Defaulter.
func (arg *Arguments) SetToDefault() {
	*arg = Arguments{
		CacheSize: 100_000,
	}
}

// Validate implements river.Validator.
func (arg *Arguments) Validate() error {
	if arg.CacheSize <= 0 {
		return fmt.Errorf("max_cache_size must be greater than 0 and is %d", arg.CacheSize)
	}
	return nil
}

// Exports holds values which are exported by the prometheus.relabel component.
type Exports struct {
	Receiver labelstore.Appendable `river:"receiver,attr"`
	Rules    flow_relabel.Rules    `river:"rules,attr"`
}

// Component implements the prometheus.relabel component.
type Component struct {
	mut              sync.RWMutex
	opts             component.Options
	mrc              []*relabel.Config
	receiver         *prometheus.Interceptor
	metricsProcessed prometheus_client.Counter
	metricsOutgoing  prometheus_client.Counter
	cacheHits        prometheus_client.Counter
	cacheMisses      prometheus_client.Counter
	cacheSize        prometheus_client.Gauge
	cacheDeletes     prometheus_client.Counter
	fanout           *prometheus.Fanout
	exited           atomic.Bool
	ls               labelstore.LabelStore

	cacheMut sync.RWMutex
	cache    *lru.Cache[uint64, *labelstore.Series]
}

var _ component.Component = (*Component)(nil)

// New creates a new prometheus.relabel component.
func New(o component.Options, args Arguments) (*Component, error) {
	cache, err := lru.New[uint64, *labelstore.Series](args.CacheSize)
	if err != nil {
		return nil, err
	}
	data, err := o.GetServiceData(labelstore.ServiceName)
	if err != nil {
		return nil, err
	}
	c := &Component{
		opts:  o,
		cache: cache,
		ls:    data.(labelstore.LabelStore),
	}
	c.metricsProcessed = prometheus_client.NewCounter(prometheus_client.CounterOpts{
		Name: "agent_prometheus_relabel_metrics_processed",
		Help: "Total number of metrics processed",
	})
	c.metricsOutgoing = prometheus_client.NewCounter(prometheus_client.CounterOpts{
		Name: "agent_prometheus_relabel_metrics_written",
		Help: "Total number of metrics written",
	})
	c.cacheMisses = prometheus_client.NewCounter(prometheus_client.CounterOpts{
		Name: "agent_prometheus_relabel_cache_misses",
		Help: "Total number of cache misses",
	})
	c.cacheHits = prometheus_client.NewCounter(prometheus_client.CounterOpts{
		Name: "agent_prometheus_relabel_cache_hits",
		Help: "Total number of cache hits",
	})
	c.cacheSize = prometheus_client.NewGauge(prometheus_client.GaugeOpts{
		Name: "agent_prometheus_relabel_cache_size",
		Help: "Total size of relabel cache",
	})
	c.cacheDeletes = prometheus_client.NewCounter(prometheus_client.CounterOpts{
		Name: "agent_prometheus_relabel_cache_deletes",
		Help: "Total number of cache deletes",
	})

	for _, metric := range []prometheus_client.Collector{c.metricsProcessed, c.metricsOutgoing, c.cacheMisses, c.cacheHits, c.cacheSize, c.cacheDeletes} {
		err = o.Registerer.Register(metric)
		if err != nil {
			return nil, err
		}
	}

	c.fanout = prometheus.NewFanout(args.ForwardTo, o.ID, o.Registerer, c.ls)
	c.receiver = prometheus.NewInterceptor(
		c.fanout,
		c.ls,
		prometheus.WithAppendHook(func(series *labelstore.Series, next labelstore.Appender) (storage.SeriesRef, error) {
			if c.exited.Load() {
				return 0, fmt.Errorf("%s has exited", o.ID)
			}

			newseries := c.relabel(series)
			if newseries.Lbls.IsEmpty() {
				return 0, nil
			}
			c.metricsOutgoing.Inc()
			return next.Append(newseries)
		}),
		prometheus.WithExemplarHook(func(series *labelstore.Series, e exemplar.Exemplar, next labelstore.Appender) (storage.SeriesRef, error) {
			if c.exited.Load() {
				return 0, fmt.Errorf("%s has exited", o.ID)
			}

			newseries := c.relabel(series)
			if newseries.Lbls.IsEmpty() {
				return 0, nil
			}
			return next.AppendExemplar(newseries, e)
		}),
		prometheus.WithMetadataHook(func(series *labelstore.Series, m metadata.Metadata, next labelstore.Appender) (storage.SeriesRef, error) {
			if c.exited.Load() {
				return 0, fmt.Errorf("%s has exited", o.ID)
			}
			newseries := c.relabel(series)
			if newseries.Lbls.IsEmpty() {
				return 0, nil
			}
			return next.UpdateMetadata(newseries, m)
		}),
		prometheus.WithHistogramHook(func(series *labelstore.Series, h *histogram.Histogram, fh *histogram.FloatHistogram, next labelstore.Appender) (storage.SeriesRef, error) {
			if c.exited.Load() {
				return 0, fmt.Errorf("%s has exited", o.ID)
			}
			newseries := c.relabel(series)
			if newseries.Lbls.IsEmpty() {
				return 0, nil
			}

			return next.AppendHistogram(newseries, h, fh)
		}),
	)

	// Immediately export the receiver which remains the same for the component
	// lifetime.
	o.OnStateChange(Exports{Receiver: c.receiver, Rules: args.MetricRelabelConfigs})

	// Call to Update() to set the relabelling rules once at the start.
	if err = c.Update(args); err != nil {
		return nil, err
	}

	return c, nil
}

// Run implements component.Component.
func (c *Component) Run(ctx context.Context) error {
	defer c.exited.Store(true)

	<-ctx.Done()
	return nil
}

// Update implements component.Component.
func (c *Component) Update(args component.Arguments) error {
	c.mut.Lock()
	defer c.mut.Unlock()

	newArgs := args.(Arguments)
	c.clearCache(newArgs.CacheSize)
	c.mrc = flow_relabel.ComponentToPromRelabelConfigs(newArgs.MetricRelabelConfigs)
	c.fanout.UpdateChildren(newArgs.ForwardTo)

	c.opts.OnStateChange(Exports{Receiver: c.receiver, Rules: newArgs.MetricRelabelConfigs})

	return nil
}

func (c *Component) relabel(series *labelstore.Series) *labelstore.Series {
	c.mut.RLock()
	defer c.mut.RUnlock()

	var (
		relabelled labels.Labels
		keep       bool
	)

	newSeries, found := c.getFromCache(series.GlobalID)
	if found {
		c.cacheHits.Inc()
		if value.IsStaleNaN(series.Value) {
			c.deleteFromCache(series.GlobalID)
		}
		return newSeries
	} else {
		// Relabel against a copy of the labels to prevent modifying the original
		// slice.
		relabelled, keep = relabel.Process(series.Lbls.Copy(), c.mrc...)
		c.cacheMisses.Inc()
		newSeries = c.ls.ConvertToSeries(series.Ts, series.Value, relabelled)
		c.addToCache(series.GlobalID, newSeries, keep)
	}
	if value.IsStaleNaN(series.Value) {
		c.deleteFromCache(series.GlobalID)
	}
	// Set the cache size to the cache.len
	// TODO(@mattdurham): Instead of setting this each time could collect on demand for better performance.
	c.cacheSize.Set(float64(c.cache.Len()))
	return newSeries
}

func (c *Component) getFromCache(id uint64) (*labelstore.Series, bool) {
	c.cacheMut.RLock()
	defer c.cacheMut.RUnlock()

	fm, found := c.cache.Get(id)
	return fm, found
}

func (c *Component) deleteFromCache(id uint64) {
	c.cacheMut.Lock()
	defer c.cacheMut.Unlock()
	c.cacheDeletes.Inc()
	c.cache.Remove(id)
}

func (c *Component) clearCache(cacheSize int) {
	c.cacheMut.Lock()
	defer c.cacheMut.Unlock()
	cache, _ := lru.New[uint64, *labelstore.Series](cacheSize)
	c.cache = cache
}

func (c *Component) addToCache(originalID uint64, series *labelstore.Series, keep bool) {
	c.cacheMut.Lock()
	defer c.cacheMut.Unlock()

	if !keep {
		c.cache.Add(originalID, nil)
		return
	}
	c.cache.Add(originalID, series)
}

// labelAndID stores both the globalrefid for the label and the id itself. We store the id so that it doesn't have
// to be recalculated again.
type labelAndID struct {
	labels labels.Labels
	id     uint64
}
