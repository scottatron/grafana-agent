// Package api implements the HTTP API used for the Grafana Agent Flow UI.
//
// The API is internal only; it is not stable and shouldn't be relied on
// externally.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/gorilla/mux"
	"github.com/grafana/agent/component"
	"github.com/grafana/agent/service/cluster"
	"github.com/prometheus/prometheus/util/httputil"
)

// FlowAPI is a wrapper around the component API.
type FlowAPI struct {
	flow    component.Provider
	cluster cluster.Cluster
}

// NewFlowAPI instantiates a new Flow API.
func NewFlowAPI(flow component.Provider, cluster cluster.Cluster) *FlowAPI {
	return &FlowAPI{flow: flow, cluster: cluster}
}

// RegisterRoutes registers all the API's routes.
func (f *FlowAPI) RegisterRoutes(urlPrefix string, r *mux.Router) {
	// NOTE(rfratto): {id:.+} is used in routes below to allow the
	// id to contain / characters, which is used by nested module IDs and
	// component IDs.

	r.Handle(path.Join(urlPrefix, "/modules/{moduleID:.+}/components"), httputil.CompressionHandler{Handler: f.listComponentsHandler()})
	r.Handle(path.Join(urlPrefix, "/components"), httputil.CompressionHandler{Handler: f.listComponentsHandler()})
	r.Handle(path.Join(urlPrefix, "/components/{id:.+}"), httputil.CompressionHandler{Handler: f.getComponentHandler()})
	r.Handle(path.Join(urlPrefix, "/peers"), httputil.CompressionHandler{Handler: f.getClusteringPeersHandler()})
	r.Handle(path.Join(urlPrefix, "/streamDatas"), httputil.CompressionHandler{Handler: f.getStreamingHandler()})
}

func (f *FlowAPI) listComponentsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// moduleID is set from the /modules/{moduleID:.+}/components route above
		// but not from the /components route.
		var moduleID string
		if vars := mux.Vars(r); vars != nil {
			moduleID = vars["moduleID"]
		}

		components, err := f.flow.ListComponents(moduleID, component.InfoOptions{
			GetHealth: true,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		bb, err := json.Marshal(components)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(bb)
	}
}

func (f *FlowAPI) getComponentHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		requestedComponent := component.ParseID(vars["id"])

		component, err := f.flow.GetComponent(requestedComponent, component.InfoOptions{
			GetHealth:    true,
			GetArguments: true,
			GetExports:   true,
			GetDebugInfo: true,
		})
		if err != nil {
			http.NotFound(w, r)
			return
		}

		bb, err := json.Marshal(component)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(bb)
	}
}

func (f *FlowAPI) getClusteringPeersHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		// TODO(@tpaschalis) Detect if clustering is disabled and propagate to
		// the Typescript code (eg. via the returned status code?).
		peers := f.cluster.Peers()
		bb, err := json.Marshal(peers)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(bb)
	}
}

var _ io.WriteCloser = (*flushWriter)(nil)

// flushWriter wraps an io.Writer with an http.Flusher to flush buffered data
// to a streaming HTTP/2 connection's request body.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (w *flushWriter) Write(data []byte) (int, error) {
	n, err := w.w.Write(data)
	w.f.Flush()
	return n, err
}

func (w *flushWriter) Close() error { return nil }

func (f *FlowAPI) getStreamingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		// TODO(@tpaschalis) Detect if clustering is disabled and propagate to
		// the Typescript code (eg. via the returned status code?).
		// peers := f.cluster.Peers()
		// bb, err := json.Marshal(peers)
		// if err != nil {
		// 	http.Error(w, err.Error(), http.StatusInternalServerError)
		// 	return
		// }
		// _, _ = w.Write(bb)

		i := 0
		go func() {
			for {
				w.Write([]byte(fmt.Sprintf("Hello there??? %d\n", i)))
				w.(http.Flusher).Flush()
				time.Sleep(500 * time.Millisecond)
				i++
				if i > 10 {
					break
				}
			}
		}()
	}
}
