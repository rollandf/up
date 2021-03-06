package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	stdlog "log"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/oklog/run"
	"github.com/pkg/errors"
	promapi "github.com/prometheus/client_golang/api"
	promapiv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/promql/parser"
	"gopkg.in/yaml.v2"
)

type labelArg []prompb.Label

func (la *labelArg) String() string {
	ls := make([]string, len(*la))
	for i, l := range *la {
		ls[i] = l.Name + "=" + l.Value
	}

	return strings.Join(ls, ", ")
}

func (la *labelArg) Set(v string) error {
	labels := strings.Split(v, ",")
	lset := make([]prompb.Label, len(labels))

	for i, l := range labels {
		parts := strings.SplitN(l, "=", 2)
		if len(parts) != 2 {
			return errors.Errorf("unrecognized label %q", l)
		}

		if !model.LabelName.IsValid(model.LabelName(parts[0])) {
			return errors.Errorf("unsupported format for label %s", l)
		}

		val, err := strconv.Unquote(parts[1])
		if err != nil {
			return errors.Wrap(err, "unquote label value")
		}

		lset[i] = prompb.Label{Name: parts[0], Value: val}
	}

	*la = lset

	return nil
}

type queryResult struct {
	Type   model.ValueType `json:"resultType"`
	Result interface{}     `json:"result"`

	v model.Value
}

func (qr *queryResult) UnmarshalJSON(b []byte) error {
	v := struct {
		Status string `json:"status"`
		Data   struct {
			Type   model.ValueType `json:"resultType"`
			Result json.RawMessage `json:"result"`
		} `json:"data"`
	}{}

	err := json.Unmarshal(b, &v)
	if err != nil {
		return err
	}

	switch v.Data.Type {
	case model.ValScalar:
		var sv model.Scalar
		err = json.Unmarshal(v.Data.Result, &sv)
		qr.v = &sv

	case model.ValVector:
		var vv model.Vector
		err = json.Unmarshal(v.Data.Result, &vv)
		qr.v = vv

	case model.ValMatrix:
		var mv model.Matrix
		err = json.Unmarshal(v.Data.Result, &mv)
		qr.v = mv

	default:
		err = fmt.Errorf("unexpected value type %q", v.Data.Type)
	}

	return err
}

type options struct {
	LogLevel          level.Option
	WriteEndpoint     *url.URL
	ReadEndpoint      *url.URL
	Labels            labelArg
	Listen            string
	Name              string
	Token             TokenProvider
	Queries           []querySpec
	Period            time.Duration
	Duration          time.Duration
	Latency           time.Duration
	InitialQueryDelay time.Duration
	SuccessThreshold  float64
}

type metrics struct {
	remoteWriteRequests     *prometheus.CounterVec
	queryResponses          *prometheus.CounterVec
	metricValueDifference   prometheus.Histogram
	customQueryExecuted     *prometheus.CounterVec
	customQueryErrors       *prometheus.CounterVec
	customQueryLastDuration *prometheus.GaugeVec
}

func main() {
	l := log.WithPrefix(log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr)), "name", "up")
	l = log.WithPrefix(l, "ts", log.DefaultTimestampUTC)
	l = log.WithPrefix(l, "caller", log.DefaultCaller)

	opts, err := parseFlags(l)
	if err != nil {
		level.Error(l).Log("msg", "could not parse command line flags", "err", err)
		os.Exit(1)
	}

	l = level.NewFilter(l, opts.LogLevel)
	l = log.WithPrefix(l, "caller", log.DefaultCaller)

	reg := prometheus.NewRegistry()
	m := registerMetrics(reg)

	g := &run.Group{}
	{
		// Signal chans must be buffered.
		sig := make(chan os.Signal, 1)
		g.Add(func() error {
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			<-sig
			level.Info(l).Log("msg", "caught interrupt")
			return nil
		}, func(_ error) {
			close(sig)
		})
	}
	// Schedule HTTP server
	{
		logger := log.With(l, "component", "http")
		router := http.NewServeMux()
		router.Handle("/metrics", promhttp.InstrumentMetricHandler(reg, promhttp.HandlerFor(reg, promhttp.HandlerOpts{})))
		router.HandleFunc("/debug/pprof/", pprof.Index)

		srv := &http.Server{Addr: opts.Listen, Handler: router}

		g.Add(func() error {
			level.Info(logger).Log("msg", "starting the HTTP server", "address", opts.Listen)
			return srv.ListenAndServe()
		}, func(err error) {
			if err == http.ErrServerClosed {
				level.Warn(logger).Log("msg", "internal server closed unexpectedly")
				return
			}
			level.Info(logger).Log("msg", "shutting down internal server")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				stdlog.Fatal(err)
			}
		})
	}

	ctx := context.Background()

	var cancel context.CancelFunc
	if opts.Duration != 0 {
		ctx, cancel = context.WithTimeout(ctx, opts.Duration)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	if opts.WriteEndpoint != nil {
		g.Add(func() error {
			l := log.With(l, "component", "writer")
			level.Info(l).Log("msg", "starting the writer")

			return runPeriodically(ctx, opts, m.remoteWriteRequests, l, func(rCtx context.Context) {
				if err := write(rCtx, opts.WriteEndpoint, opts.Token, generate(opts.Labels), l); err != nil {
					m.remoteWriteRequests.WithLabelValues("error").Inc()
					level.Error(l).Log("msg", "failed to make request", "err", err)
				} else {
					m.remoteWriteRequests.WithLabelValues("success").Inc()
				}
			})
		}, func(_ error) {
			cancel()
		})
	}

	if opts.ReadEndpoint != nil && opts.WriteEndpoint != nil {
		g.Add(func() error {
			l := log.With(l, "component", "reader")
			level.Info(l).Log("msg", "starting the reader")

			// Wait for at least one period before start reading metrics.
			level.Info(l).Log("msg", "waiting for initial delay before querying for metrics")
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(opts.InitialQueryDelay):
			}

			level.Info(l).Log("msg", "start querying for metrics")

			return runPeriodically(ctx, opts, m.queryResponses, l, func(rCtx context.Context) {
				if err := read(rCtx, opts.ReadEndpoint, opts.Labels, -1*opts.InitialQueryDelay, opts.Latency, m); err != nil {
					m.queryResponses.WithLabelValues("error").Inc()
					level.Error(l).Log("msg", "failed to query", "err", err)
				} else {
					m.queryResponses.WithLabelValues("success").Inc()
				}
			})
		}, func(_ error) {
			cancel()
		})
	}

	if opts.ReadEndpoint != nil && opts.Queries != nil {
		addCustomQueryRunGroup(ctx, g, l, opts, m, cancel)
	}

	if err := g.Run(); err != nil {
		level.Error(l).Log("msg", "run group exited with error", "err", err)
		os.Exit(1)
	}

	level.Info(l).Log("msg", "up completed its mission!")
}

func addCustomQueryRunGroup(ctx context.Context, g *run.Group, l log.Logger, opts options, m metrics, cancel func()) {
	g.Add(func() error {
		l := log.With(l, "component", "query-reader")
		level.Info(l).Log("msg", "starting the reader for queries")

		// Wait for at least one period before start reading metrics.
		level.Info(l).Log("msg", "waiting for initial delay before querying specified queries")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.InitialQueryDelay):
		}

		level.Info(l).Log("msg", "start querying for specified queries")

		for {
			select {
			case <-ctx.Done():
				return nil
			default:
				for _, q := range opts.Queries {
					select {
					case <-ctx.Done():
						return nil
					default:
						t := time.Now()
						warn, err := query(
							ctx,
							l,
							opts.ReadEndpoint,
							opts.Token,
							q,
						)
						duration := time.Since(t).Seconds()
						if err != nil {
							level.Info(l).Log(
								"msg", "failed to execute specified query",
								"name", q.Name,
								"duration", duration,
								"warnings", fmt.Sprintf("%#+v", warn),
								"err", err,
							)
							m.customQueryErrors.WithLabelValues(q.Name).Inc()
						} else {
							level.Debug(l).Log("msg", "successfully executed specified query",
								"name", q.Name,
								"duration", duration,
								"warnings", fmt.Sprintf("%#+v", warn),
							)
							m.customQueryLastDuration.WithLabelValues(q.Name).Set(duration)
						}
						m.customQueryExecuted.WithLabelValues(q.Name).Inc()
					}
				}
			}
		}
	}, func(_ error) {
		cancel()
	})
}

func runPeriodically(ctx context.Context, opts options, c *prometheus.CounterVec, l log.Logger, f func(rCtx context.Context)) error {
	var (
		t        = time.NewTicker(opts.Period)
		deadline time.Time
		rCtx     context.Context
		rCancel  context.CancelFunc
	)

	for {
		select {
		case <-t.C:
			// NOTICE: Do not propagate parent context to prevent cancellation of in-flight request.
			// It will be cancelled after the deadline.
			deadline = time.Now().Add(opts.Period)
			rCtx, rCancel = context.WithDeadline(context.Background(), deadline)

			// Will only get scheduled once per period and guaranteed to get cancelled after deadline.
			go func() {
				defer rCancel() // Make sure context gets cancelled even if execution panics.

				f(rCtx)
			}()
		case <-ctx.Done():
			t.Stop()

			select {
			// If it gets immediately cancelled, zero value of deadline won't cause a lock!
			case <-time.After(time.Until(deadline)):
				rCancel()
			case <-rCtx.Done():
			}

			return reportResults(l, c, opts.SuccessThreshold)
		}
	}
}

type TokenProvider interface {
	Get() (string, error)
}

type instantQueryRoundTripper struct {
	l       log.Logger
	r       http.RoundTripper
	t       TokenProvider
	TraceID string
}

func newInstantQueryRoundTripper(l log.Logger, t TokenProvider, r http.RoundTripper) *instantQueryRoundTripper {
	if r == nil {
		r = http.DefaultTransport
	}

	return &instantQueryRoundTripper{
		l: l,
		t: t,
		r: r,
	}
}

func (r *instantQueryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := r.t.Get()
	if err != nil {
		return nil, err
	}

	if token != "" {
		req.Header.Add("Authorization", "Bearer "+token)
	}

	resp, err := r.r.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	r.TraceID = resp.Header.Get("X-Thanos-Trace-Id")

	return resp, err
}

func query(
	ctx context.Context,
	l log.Logger,
	endpoint *url.URL,
	t TokenProvider,
	query querySpec,
) (promapiv1.Warnings, error) {
	var (
		warn promapiv1.Warnings
		err  error
	)

	level.Debug(l).Log("msg", "running specified query", "name", query.Name, "query", query.Query)

	// Copy URL to avoid modifying the passed value.
	u := new(url.URL)
	*u = *endpoint
	u.Path = ""

	r := newInstantQueryRoundTripper(l, t, nil)

	c, err := promapi.NewClient(promapi.Config{
		Address:      u.String(),
		RoundTripper: r,
	})
	if err != nil {
		err = fmt.Errorf("create new API client: %w", err)
		return warn, err
	}

	a := promapiv1.NewAPI(c)

	var res model.Value

	res, warn, err = a.Query(ctx, query.Query, time.Now())
	if err != nil {
		err = fmt.Errorf("querying: %w", err)
		return warn, err
	}

	level.Debug(l).Log("msg", "request finished", "name", query.Name, "response", res.String(), "trace-id", r.TraceID)

	return warn, err
}

// doGetFallback will attempt to do the request as-is, and on a 405 it will fallback to a GET request.
// Copied from the prometheus API client v1.2.1 (as it was removed afterwards).
// https://github.com/prometheus/client_golang/blob/55450579111f95e3722cb93dec62fe9e847d6130/api/client.go#L64
func doGetFallback(ctx context.Context, c promapi.Client, u *url.URL, args url.Values) (*http.Response, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, u.String(), strings.NewReader(args.Encode()))
	if err != nil {
		return nil, nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, body, err := c.Do(ctx, req)
	if resp != nil && resp.StatusCode == http.StatusMethodNotAllowed {
		u.RawQuery = args.Encode()
		req, err = http.NewRequest(http.MethodGet, u.String(), nil)

		if err != nil {
			return nil, nil, err
		}
	} else {
		if err != nil {
			return resp, body, err
		}

		return resp, body, nil
	}

	return c.Do(ctx, req)
}

func read(ctx context.Context, endpoint *url.URL, labels []prompb.Label, ago, latency time.Duration, m metrics) error {
	client, err := promapi.NewClient(promapi.Config{Address: endpoint.String()})
	if err != nil {
		return err
	}

	labelSelectors := make([]string, len(labels))
	for i, label := range labels {
		labelSelectors[i] = fmt.Sprintf(`%s="%s"`, label.Name, label.Value)
	}

	query := fmt.Sprintf("{%s}", strings.Join(labelSelectors, ","))

	q := endpoint.Query()
	q.Set("query", query)

	ts := time.Now().Add(ago)
	if !ts.IsZero() {
		q.Set("time", formatTime(ts))
	}

	_, body, err := doGetFallback(ctx, client, endpoint, q) //nolint:bodyclose
	if err != nil {
		return errors.Wrap(err, "query request failed")
	}

	var result queryResult
	if err := json.Unmarshal(body, &result); err != nil {
		return errors.Wrap(err, "query response parse failed")
	}

	vec := result.v.(model.Vector)
	if len(vec) != 1 {
		return fmt.Errorf("expected one metric, got %d", len(vec))
	}

	t := time.Unix(int64(vec[0].Value/1000), 0)

	diffSeconds := time.Since(t).Seconds()

	m.metricValueDifference.Observe(diffSeconds)

	if diffSeconds > latency.Seconds() {
		return fmt.Errorf("metric value is too old: %2.fs", diffSeconds)
	}

	return nil
}

func write(ctx context.Context, endpoint fmt.Stringer, t TokenProvider, wreq proto.Message, l log.Logger) error {
	var (
		buf []byte
		err error
		req *http.Request
		res *http.Response
	)

	buf, err = proto.Marshal(wreq)
	if err != nil {
		return errors.Wrap(err, "marshalling proto")
	}

	req, err = http.NewRequest("POST", endpoint.String(), bytes.NewBuffer(snappy.Encode(nil, buf)))
	if err != nil {
		return errors.Wrap(err, "creating request")
	}

	token, err := t.Get()
	if err != nil {
		return errors.Wrap(err, "retrieving token")
	}

	if token != "" {
		req.Header.Add("Authorization", "Bearer "+token)
	}

	res, err = http.DefaultClient.Do(req.WithContext(ctx)) //nolint:bodyclose
	if err != nil {
		return errors.Wrap(err, "making request")
	}

	defer exhaustCloseWithLogOnErr(l, res.Body)

	if res.StatusCode != http.StatusOK {
		err = errors.New(res.Status)
		return errors.Wrap(err, "non-200 status")
	}

	return nil
}

func reportResults(l log.Logger, c *prometheus.CounterVec, threshold float64) error {
	metrics := make(chan prometheus.Metric, 2)
	c.Collect(metrics)
	close(metrics)

	var success, errors float64

	for m := range metrics {
		m1 := &dto.Metric{}
		if err := m.Write(m1); err != nil {
			level.Warn(l).Log("msg", "cannot read success and error count from prometheus counter", "err", err)
		}

		for _, l := range m1.Label {
			switch *l.Value {
			case "error":
				errors = m1.GetCounter().GetValue()
			case "success":
				success = m1.GetCounter().GetValue()
			}
		}
	}

	level.Info(l).Log("msg", "number of requests", "success", success, "errors", errors)

	ratio := success / (success + errors)
	if ratio < threshold {
		level.Error(l).Log("msg", "ratio is below threshold")
		return fmt.Errorf("failed with less than %2.f%% success ratio - actual %2.f%%", threshold*100, ratio*100)
	}

	return nil
}

func generate(labels []prompb.Label) *prompb.WriteRequest {
	timestamp := time.Now().UnixNano() / int64(time.Millisecond)

	return &prompb.WriteRequest{
		Timeseries: []prompb.TimeSeries{
			{
				Labels: labels,
				Samples: []prompb.Sample{
					{
						Value:     float64(timestamp),
						Timestamp: timestamp,
					},
				},
			},
		},
	}
}

type querySpec struct {
	Name  string `yaml:"name"`
	Query string `yaml:"query"`
}

type queriesFile struct {
	Queries []querySpec `yaml:"queries"`
}

// Helpers
func parseFlags(l log.Logger) (options, error) {
	var (
		rawWriteEndpoint string
		rawReadEndpoint  string
		rawLogLevel      string
		queriesFileName  string
		tokenFile        string
		token            string
	)

	opts := options{}

	flag.StringVar(&rawLogLevel, "log.level", "info", "The log filtering level. Options: 'error', 'warn', 'info', 'debug'.")
	flag.StringVar(&rawWriteEndpoint, "endpoint-write", "", "The endpoint to which to make remote-write requests.")
	flag.StringVar(&rawReadEndpoint, "endpoint-read", "", "The endpoint to which to make query requests.")
	flag.Var(&opts.Labels, "labels", "The labels in addition to '__name__' that should be applied to remote-write requests.")
	flag.StringVar(&opts.Listen, "listen", ":8080", "The address on which internal server runs.")
	flag.StringVar(&opts.Name, "name", "up", "The name of the metric to send in remote-write requests.")
	flag.StringVar(&token, "token", "",
		"The bearer token to set in the authorization header on remote-write requests. Takes predence over --token-file if set.")
	flag.StringVar(&tokenFile, "token-file", "",
		"The file to read a bearer token from and set in the authorization header on remote-write requests.")
	flag.StringVar(&queriesFileName, "queries-file", "", "A file containing queries to run against the read endpoint.")
	flag.DurationVar(&opts.Period, "period", 5*time.Second, "The time to wait between remote-write requests.")
	flag.DurationVar(&opts.Duration, "duration", 5*time.Minute,
		"The duration of the up command to run until it stops. If 0 it will not stop until the process is terminated.")
	flag.Float64Var(&opts.SuccessThreshold, "threshold", 0.9, "The percentage of successful requests needed to succeed overall. 0 - 1.")
	flag.DurationVar(&opts.Latency, "latency", 15*time.Second, "The maximum allowable latency between writing and reading.")
	flag.DurationVar(&opts.InitialQueryDelay, "initial-query-delay", 5*time.Second, "The time to wait before executing the first query.")
	flag.Parse()

	return buildOptionsFromFlags(l, opts, rawLogLevel, rawWriteEndpoint, rawReadEndpoint, queriesFileName, token, tokenFile)
}

func buildOptionsFromFlags(
	l log.Logger,
	opts options,
	rawLogLevel, rawWriteEndpoint, rawReadEndpoint, queriesFileName, token, tokenFile string,
) (options, error) {
	var err error

	switch rawLogLevel {
	case "error":
		opts.LogLevel = level.AllowError()
	case "warn":
		opts.LogLevel = level.AllowWarn()
	case "info":
		opts.LogLevel = level.AllowInfo()
	case "debug":
		opts.LogLevel = level.AllowDebug()
	default:
		panic("unexpected log level")
	}

	if rawWriteEndpoint != "" {
		writeEndpoint, err := url.ParseRequestURI(rawWriteEndpoint)
		if err != nil {
			return opts, fmt.Errorf("--endpoint-write is invalid: %w", err)
		}

		opts.WriteEndpoint = writeEndpoint
	} else {
		l.Log("msg", "no write endpoint specified, no write tests being performed")
	}

	if rawReadEndpoint != "" {
		var readEndpoint *url.URL
		if rawReadEndpoint != "" {
			readEndpoint, err = url.ParseRequestURI(rawReadEndpoint)
			if err != nil {
				return opts, fmt.Errorf("--endpoint-read is invalid: %w", err)
			}
		}

		opts.ReadEndpoint = readEndpoint
	} else {
		l.Log("msg", "no read endpoint specified, no read tests being performed")
	}

	if queriesFileName != "" {
		b, err := ioutil.ReadFile(queriesFileName)
		if err != nil {
			return opts, fmt.Errorf("--queries-file is invalid: %w", err)
		}

		qf := queriesFile{}
		err = yaml.Unmarshal(b, &qf)

		if err != nil {
			return opts, fmt.Errorf("--queries-file content is invalid: %w", err)
		}

		l.Log("msg", fmt.Sprintf("%d queries configured to be queried periodically", len(qf.Queries)))

		// validate queries
		for _, q := range qf.Queries {
			_, err = parser.ParseExpr(q.Query)
			if err != nil {
				return opts, fmt.Errorf("query %q in --queries-file content is invalid: %w", q.Name, err)
			}
		}

		opts.Queries = qf.Queries
	}

	if opts.Latency <= opts.Period {
		return opts, errors.New("--latency cannot be less than period")
	}

	opts.Labels = append(opts.Labels, prompb.Label{
		Name:  "__name__",
		Value: opts.Name,
	})

	opts.Token = tokenProvider(token, tokenFile)

	return opts, err
}

func tokenProvider(token, tokenFile string) TokenProvider {
	var res TokenProvider

	res = NewNoOpTokenProvider()
	if tokenFile != "" {
		res = NewFileToken(tokenFile)
	}

	if token != "" {
		res = NewStaticToken(token)
	}

	return res
}

func registerMetrics(reg *prometheus.Registry) metrics {
	m := metrics{
		remoteWriteRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "up_remote_writes_total",
			Help: "Total number of remote write requests.",
		}, []string{"result"}),
		queryResponses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "up_queries_total",
			Help: "The total number of queries made.",
		}, []string{"result"}),
		metricValueDifference: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "up_metric_value_difference",
			Help:    "The time difference between the current timestamp and the timestamp in the metrics value.",
			Buckets: prometheus.LinearBuckets(4, 0.25, 16),
		}),
		customQueryExecuted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "up_custom_query_executed_total",
			Help: "The total number of custom specified queries executed.",
		}, []string{"query"}),
		customQueryErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "up_custom_query_errors_total",
			Help: "The total number of custom specified queries executed.",
		}, []string{"query"}),
		customQueryLastDuration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "up_custom_query_last_duration",
			Help: "The duration of the query execution last time the query was executed successfully.",
		}, []string{"query"}),
	}
	reg.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
		m.remoteWriteRequests,
		m.queryResponses,
		m.metricValueDifference,
		m.customQueryExecuted,
		m.customQueryErrors,
		m.customQueryLastDuration,
	)

	return m
}

func exhaustCloseWithLogOnErr(l log.Logger, rc io.ReadCloser) {
	if _, err := io.Copy(ioutil.Discard, rc); err != nil {
		level.Warn(l).Log("msg", "failed to exhaust reader, performance may be impeded", "err", err)
	}

	if err := rc.Close(); err != nil {
		level.Warn(l).Log("msg", "detected close error", "err", errors.Wrap(err, "response body close"))
	}
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.Unix())+float64(t.Nanosecond())/1e9, 'f', -1, 64)
}
