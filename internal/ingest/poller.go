package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	gtfsrt "github.com/briankim06/urban-goggles/proto/gtfsrt"
	transit "github.com/briankim06/urban-goggles/proto/transit"
	"google.golang.org/protobuf/proto"
)

// Publisher is the minimal interface a Poller needs to emit normalized delay
// events to downstream infrastructure (Kafka in production, an in-memory
// channel in tests).
type Publisher interface {
	Publish(ctx context.Context, ev *transit.DelayEvent) error
}

// Poller periodically fetches a single GTFS-RT feed URL, deserializes the
// protobuf payload, diffs it against the previous snapshot, and publishes any
// meaningfully changed DelayEvents.
type Poller struct {
	Name     string
	URL      string
	AgencyID string
	Interval time.Duration

	HTTPClient *http.Client
	Publisher  Publisher
	Differ     *Differ
	Logger     *slog.Logger

	published uint64 // atomic counter of events published since start
}

// NewPoller constructs a Poller with a 10 second HTTP timeout by default.
func NewPoller(name, url, agencyID string, interval time.Duration, pub Publisher, logger *slog.Logger) *Poller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{
		Name:       name,
		URL:        url,
		AgencyID:   agencyID,
		Interval:   interval,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		Publisher:  pub,
		Differ:     NewDiffer(),
		Logger:     logger.With("feed", name),
	}
}

// Published returns the running count of events this poller has handed to the
// Publisher since Start was invoked. Used by the ingestor's throughput logger.
func (p *Poller) Published() uint64 {
	return atomic.LoadUint64(&p.published)
}

// Start blocks until ctx is cancelled. It performs an immediate first poll and
// then polls on a ticker at Interval. Errors from individual polls are logged
// and do not stop the loop.
func (p *Poller) Start(ctx context.Context) {
	p.Logger.Info("poller starting", "url", p.URL, "interval", p.Interval)
	p.pollOnce(ctx)

	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.Logger.Info("poller stopping")
			return
		case <-t.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	feed, err := p.fetch(ctx)
	if err != nil {
		p.Logger.Warn("feed fetch failed", "err", err)
		return
	}
	observedAt := int64(feed.GetHeader().GetTimestamp())
	if observedAt == 0 {
		observedAt = time.Now().Unix()
	}

	var raw []*transit.DelayEvent
	for _, entity := range feed.GetEntity() {
		tu := entity.GetTripUpdate()
		if tu == nil {
			continue
		}
		raw = append(raw, NormalizeTripUpdate(p.AgencyID, observedAt, tu)...)
	}

	changed := p.Differ.Diff(raw)
	for _, ev := range changed {
		if err := p.Publisher.Publish(ctx, ev); err != nil {
			p.Logger.Error("publish failed", "err", err, "trip", ev.GetTripId(), "stop", ev.GetStopId())
			continue
		}
		atomic.AddUint64(&p.published, 1)
	}
	p.Logger.Debug("poll complete", "raw", len(raw), "emitted", len(changed))
}

func (p *Poller) fetch(ctx context.Context) (*gtfsrt.FeedMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var msg gtfsrt.FeedMessage
	if err := proto.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal feed: %w", err)
	}
	return &msg, nil
}
