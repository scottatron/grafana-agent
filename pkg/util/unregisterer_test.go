package util

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func Test_UnregisterTwice(t *testing.T) {
	u := WrapWithUnregisterer(prometheus.NewRegistry())
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_metric",
		Help: "Test metric.",
	})
	u.Register(c)
	require.True(t, u.Unregister(c))
	require.True(t, u.Unregister(c))
}
