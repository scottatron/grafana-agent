package configstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/gorilla/mux"
	"github.com/grafana/agent/internal/static/client"
	"github.com/grafana/agent/internal/static/metrics/cluster/configapi"
	"github.com/grafana/agent/internal/static/metrics/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPI_ListConfigurations(t *testing.T) {
	s := &Mock{
		ListFunc: func(ctx context.Context) ([]string, error) {
			return []string{"a", "b", "c"}, nil
		},
	}

	api := NewAPI(log.NewNopLogger(), s, nil, true)
	env := newAPITestEnvironment(t, api)

	resp, err := http.Get(env.srv.URL + "/agent/api/v1/configs")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	expect := `{
		"status": "success",
		"data": {
			"configs": ["a", "b", "c"]
		}
	}`
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.JSONEq(t, expect, string(body))

	t.Run("With Client", func(t *testing.T) {
		cli := client.New(env.srv.URL)
		apiResp, err := cli.ListConfigs(context.Background())
		require.NoError(t, err)

		expect := &configapi.ListConfigurationsResponse{Configs: []string{"a", "b", "c"}}
		require.Equal(t, expect, apiResp)
	})
}

func TestAPI_GetConfiguration_Invalid(t *testing.T) {
	s := &Mock{
		GetFunc: func(ctx context.Context, key string) (instance.Config, error) {
			return instance.Config{}, NotExistError{Key: key}
		},
	}

	api := NewAPI(log.NewNopLogger(), s, nil, true)
	env := newAPITestEnvironment(t, api)

	resp, err := http.Get(env.srv.URL + "/agent/api/v1/configs/does-not-exist")
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	expect := `{
		"status": "error",
		"data": {
			"error": "configuration does-not-exist does not exist"
		}
	}`
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.JSONEq(t, expect, string(body))

	t.Run("With Client", func(t *testing.T) {
		cli := client.New(env.srv.URL)
		_, err := cli.GetConfiguration(context.Background(), "does-not-exist")
		require.NotNil(t, err)
		require.Equal(t, "configuration does-not-exist does not exist", err.Error())
	})
}

func TestAPI_GetConfiguration(t *testing.T) {
	s := &Mock{
		GetFunc: func(ctx context.Context, key string) (instance.Config, error) {
			return instance.Config{
				Name:                key,
				HostFilter:          true,
				RemoteFlushDeadline: 10 * time.Minute,
			}, nil
		},
	}

	api := NewAPI(log.NewNopLogger(), s, nil, true)
	env := newAPITestEnvironment(t, api)

	resp, err := http.Get(env.srv.URL + "/agent/api/v1/configs/exists")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	expect := `{
		"status": "success",
		"data": {
			"value": "name: exists\nhost_filter: true\nremote_flush_deadline: 10m0s\n"
		}
	}`
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.JSONEq(t, expect, string(body))

	t.Run("With Client", func(t *testing.T) {
		cli := client.New(env.srv.URL)
		actual, err := cli.GetConfiguration(context.Background(), "exists")
		require.NoError(t, err)

		// The client will apply defaults, so we need to start with the DefaultConfig
		// as a base here.
		expect := instance.DefaultConfig
		expect.Name = "exists"
		expect.HostFilter = true
		expect.RemoteFlushDeadline = 10 * time.Minute
		require.Equal(t, &expect, actual)
	})
}

func TestAPI_GetConfiguration_ScrubSecrets(t *testing.T) {
	rawConfig := `name: exists
scrape_configs:
- job_name: local_scrape
  follow_redirects: true
  enable_compression: true
  enable_http2: true
  honor_timestamps: true
  metrics_path: /metrics
  scheme: http
  track_timestamps_staleness: true
  static_configs:
  - targets:
    - 127.0.0.1:12345
    labels:
      cluster: localhost
  basic_auth:
    username: admin
    password: SCRUBME
remote_write:
- url: http://localhost:9009/api/prom/push
  remote_timeout: 30s
  name: test-d0f32c
  send_exemplars: true
  basic_auth:
    username: admin
    password: SCRUBME
  queue_config:
    capacity: 500
    max_shards: 1000
    min_shards: 1
    max_samples_per_send: 100
    batch_send_deadline: 5s
    min_backoff: 30ms
    max_backoff: 100ms
    retry_on_http_429: true
  follow_redirects: true
  enable_http2: true
  metadata_config:
    send: true
    send_interval: 1m
    max_samples_per_send: 500
wal_truncate_frequency: 1m0s
min_wal_time: 5m0s
max_wal_time: 4h0m0s
remote_flush_deadline: 1m0s
`
	scrubbedConfig := strings.ReplaceAll(rawConfig, "SCRUBME", "<secret>")

	s := &Mock{
		GetFunc: func(ctx context.Context, key string) (instance.Config, error) {
			c, err := instance.UnmarshalConfig(strings.NewReader(rawConfig))
			if err != nil {
				return instance.Config{}, err
			}
			return *c, nil
		},
	}

	api := NewAPI(log.NewNopLogger(), s, nil, true)
	env := newAPITestEnvironment(t, api)

	resp, err := http.Get(env.srv.URL + "/agent/api/v1/configs/exists")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	respBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var apiResp struct {
		Status string `json:"status"`
		Data   struct {
			Value string `json:"value"`
		} `json:"data"`
	}
	err = json.Unmarshal(respBytes, &apiResp)
	require.NoError(t, err)
	require.Equal(t, "success", apiResp.Status)
	require.YAMLEq(t, scrubbedConfig, apiResp.Data.Value)

	t.Run("With Client", func(t *testing.T) {
		cli := client.New(env.srv.URL)
		actual, err := cli.GetConfiguration(context.Background(), "exists")
		require.NoError(t, err)

		// Marshal the retrieved config _without_ scrubbing. This means
		// that if the secrets weren't scrubbed from GetConfiguration, something
		// bad happened at the API level.
		actualBytes, err := instance.MarshalConfig(actual, false)
		require.NoError(t, err)
		require.YAMLEq(t, scrubbedConfig, string(actualBytes))
	})
}

func TestServer_GetConfiguration_Disabled(t *testing.T) {
	api := NewAPI(log.NewNopLogger(), nil, nil, false)
	env := newAPITestEnvironment(t, api)
	resp, err := http.Get(env.srv.URL + "/agent/api/v1/configs/exists")
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, []byte("404 - config endpoint is disabled"), body)
}

func TestServer_PutConfiguration(t *testing.T) {
	var s Mock

	api := NewAPI(log.NewNopLogger(), &s, nil, true)
	env := newAPITestEnvironment(t, api)

	cfg := instance.Config{Name: "newconfig"}
	bb, err := instance.MarshalConfig(&cfg, false)
	require.NoError(t, err)

	t.Run("Created", func(t *testing.T) {
		// Created configs should return http.StatusCreated
		s.PutFunc = func(ctx context.Context, c instance.Config) (created bool, err error) {
			return true, nil
		}

		resp, err := http.Post(env.srv.URL+"/agent/api/v1/config/newconfig", "", bytes.NewReader(bb))
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
	})

	t.Run("Updated", func(t *testing.T) {
		// Updated configs should return http.StatusOK
		s.PutFunc = func(ctx context.Context, c instance.Config) (created bool, err error) {
			return false, nil
		}

		resp, err := http.Post(env.srv.URL+"/agent/api/v1/config/newconfig", "", bytes.NewReader(bb))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

func TestServer_PutConfiguration_Invalid(t *testing.T) {
	var s Mock

	api := NewAPI(log.NewNopLogger(), &s, func(c *instance.Config) error {
		return fmt.Errorf("custom validation error")
	}, true)
	env := newAPITestEnvironment(t, api)

	cfg := instance.Config{Name: "newconfig"}
	bb, err := instance.MarshalConfig(&cfg, false)
	require.NoError(t, err)

	resp, err := http.Post(env.srv.URL+"/agent/api/v1/config/newconfig", "", bytes.NewReader(bb))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	expect := `{
		"status": "error",
		"data": {
			"error": "failed to validate config: custom validation error"
		}
	}`
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.JSONEq(t, expect, string(body))
}

func TestServer_PutConfiguration_WithClient(t *testing.T) {
	var s Mock
	api := NewAPI(log.NewNopLogger(), &s, nil, true)
	env := newAPITestEnvironment(t, api)

	cfg := instance.DefaultConfig
	cfg.Name = "newconfig-withclient"
	cfg.HostFilter = true
	cfg.RemoteFlushDeadline = 10 * time.Minute

	s.PutFunc = func(ctx context.Context, c instance.Config) (created bool, err error) {
		assert.Equal(t, cfg, c)
		return true, nil
	}

	cli := client.New(env.srv.URL)
	err := cli.PutConfiguration(context.Background(), "newconfig-withclient", &cfg)
	require.NoError(t, err)
}

func TestServer_DeleteConfiguration(t *testing.T) {
	s := &Mock{
		DeleteFunc: func(ctx context.Context, key string) error {
			assert.Equal(t, "deleteme", key)
			return nil
		},
	}

	api := NewAPI(log.NewNopLogger(), s, nil, true)
	env := newAPITestEnvironment(t, api)

	req, err := http.NewRequest(http.MethodDelete, env.srv.URL+"/agent/api/v1/config/deleteme", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	t.Run("With Client", func(t *testing.T) {
		cli := client.New(env.srv.URL)
		err := cli.DeleteConfiguration(context.Background(), "deleteme")
		require.NoError(t, err)
	})
}

func TestServer_DeleteConfiguration_Invalid(t *testing.T) {
	s := &Mock{
		DeleteFunc: func(ctx context.Context, key string) error {
			assert.Equal(t, "deleteme", key)
			return NotExistError{Key: key}
		},
	}

	api := NewAPI(log.NewNopLogger(), s, nil, true)
	env := newAPITestEnvironment(t, api)

	req, err := http.NewRequest(http.MethodDelete, env.srv.URL+"/agent/api/v1/config/deleteme", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	t.Run("With Client", func(t *testing.T) {
		cli := client.New(env.srv.URL)
		err := cli.DeleteConfiguration(context.Background(), "deleteme")
		require.Error(t, err)
	})
}

func TestServer_URLEncoded(t *testing.T) {
	var s Mock

	api := NewAPI(log.NewNopLogger(), &s, nil, true)
	env := newAPITestEnvironment(t, api)

	var cfg instance.Config
	bb, err := instance.MarshalConfig(&cfg, false)
	require.NoError(t, err)

	s.PutFunc = func(ctx context.Context, c instance.Config) (created bool, err error) {
		assert.Equal(t, "url/encoded", c.Name)
		return true, nil
	}

	resp, err := http.Post(env.srv.URL+"/agent/api/v1/config/url%2Fencoded", "", bytes.NewReader(bb))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	s.GetFunc = func(ctx context.Context, key string) (instance.Config, error) {
		assert.Equal(t, "url/encoded", key)
		return instance.Config{Name: "url/encoded"}, nil
	}

	resp, err = http.Get(env.srv.URL + "/agent/api/v1/configs/url%2Fencoded")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

type apiTestEnvironment struct {
	srv    *httptest.Server
	router *mux.Router
}

func newAPITestEnvironment(t *testing.T, api *API) apiTestEnvironment {
	t.Helper()

	router := mux.NewRouter()
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	api.WireAPI(router)

	return apiTestEnvironment{srv: srv, router: router}
}
