package fintech

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"sort"
	"strings"
	"time"
)

// defaultReviewHorizons are the forecast horizons used for a position that
// is only held, not watched. Watchlist entries keep their own configured
// horizons instead.
var defaultReviewHorizons = []int{5, 14, 30}

const (
	// reviewDelay paces the cycle so a large portfolio stays inside a
	// market-data provider's rate limit (Finnhub's free tier allows 60
	// requests/minute and each review makes three). Same stance as the
	// media module's enrichDelay.
	reviewDelay = time.Second

	// maxReviewsPerCycle caps how much Anthropic and market-data quota a
	// single cycle can spend. A portfolio larger than this is reviewed
	// across successive cycles rather than in one unbounded burst.
	maxReviewsPerCycle = 50

	// reviewNewsLimit is how many recent headlines are fed to the review
	// prompt, matching CreateForecast/EnrichForecast.
	reviewNewsLimit = 10
)

// Rating is the periodic review's verdict for one symbol: a five-point
// scale from "sell as much as you can" to "buy as much as you can". It is
// deliberately coarse — the point is to turn a price range and a wall of
// analysis into one comparable, sortable signal.
type Rating string

const (
	RatingMaxSell Rating = "max_sell"
	RatingSell    Rating = "sell"
	RatingHold    Rating = "hold"
	RatingBuy     Rating = "buy"
	RatingMaxBuy  Rating = "max_buy"
)

// ReviewSource records why a symbol was included in a review cycle.
type ReviewSource string

const (
	ReviewSourceWatchlist ReviewSource = "watchlist"
	ReviewSourceHolding   ReviewSource = "holding"
)

// Review is one symbol's verdict from one review cycle. Symbol and Rating
// are stored on the row rather than joined, because a review is a
// point-in-time report record that must still read correctly after its
// forecast is deleted.
type Review struct {
	ID           int64
	InstrumentID int64
	ForecastID   *int64
	Symbol       string
	Source       ReviewSource
	Rating       Rating
	Rationale    string
	ReviewedAt   time.Time
	ReportedAt   *time.Time
}

// ReviewConfig controls when the review scheduler may email its digest.
// The quiet window ("night hours") suppresses only the email — the cycle
// still runs and still stores its results, and the next send outside the
// window picks up everything that went unreported.
type ReviewConfig struct {
	AlertEnabled bool
	QuietEnabled bool
	QuietStart   string // "HH:MM"
	QuietEnd     string // "HH:MM"; wraps midnight when earlier than QuietStart
	Timezone     string // IANA name; empty means the server's local time
}

// Alerter delivers a plaintext notification. It is the narrow seam the
// review cycle needs to send email without fintech importing twire —
// the same stance as the Forecaster interface. The concrete
// implementation adapts *twire.Service in app.go, so portfolio digests
// reuse the SMTP credentials and recipient list already configured there.
type Alerter interface {
	Deliverable(ctx context.Context) bool
	Send(ctx context.Context, subject, body string) error
}

// HeldPosition is one open position, as derived by the ledger replay. It is a
// thin wrapper rather than a bare Holding because the review cycle asks a
// different question than the portfolio page — "what is held at all" — and the
// distinct type is what keeps the two from being passed to each other.
type HeldPosition struct {
	Holding
}

// reviewTarget is one unit of work in a cycle: a symbol, the horizons to
// forecast it over, and — when it is actually held — the position context that
// makes a sell-side verdict mean anything.
type reviewTarget struct {
	instrumentID int64
	symbol       string
	horizons     []int
	source       ReviewSource
	watchlistID  int64    // 0 when the symbol is held but not watched
	position     *Holding // nil when the symbol is watched but not held
}

// emitPositionReviewSchema is the JSON Schema for the synthetic
// "emit_position_review" tool. It returns the same qualitative fields as
// emit_forecast_enrichment plus a required verdict, so one call both
// refreshes the deep dive and produces the rating.
var emitPositionReviewSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "description": "A few sentences of deeper written analysis of this position right now, citing the news considered."},
		"price_range_challenge": {"type": "string", "description": "Critically assess the predicted price range for each horizon. State whether you agree or challenge it — if you disagree, explain whether the range should be wider, narrower, higher, or lower and why. Be direct: do not simply restate the range."},
		"catalysts": {"type": "array", "items": {"type": "string"}, "description": "Specific upcoming events that could move the price within the forecast horizons."},
		"risks": {"type": "array", "items": {"type": "string"}, "description": "Key risks that could invalidate the forecast."},
		"supporting_signals": {"type": "array", "items": {"type": "string"}, "description": "Evidence supporting the predicted direction(s)."},
		"conflicting_signals": {"type": "array", "items": {"type": "string"}, "description": "Evidence conflicting with the predicted direction(s)."},
		"rating": {"type": "string", "enum": ["max_sell", "sell", "hold", "buy", "max_buy"], "description": "The action verdict. Use max_sell/max_buy only for high-conviction calls where the evidence is strong; prefer hold when the evidence is mixed."},
		"rating_rationale": {"type": "string", "description": "Two or three sentences justifying the rating specifically, citing the decisive evidence."}
	},
	"required": ["summary", "price_range_challenge", "rating", "rating_rationale"]
}`)

type positionReviewToolOutput struct {
	Summary             string   `json:"summary"`
	PriceRangeChallenge string   `json:"price_range_challenge"`
	Catalysts           []string `json:"catalysts"`
	Risks               []string `json:"risks"`
	SupportingSignals   []string `json:"supporting_signals"`
	ConflictingSignals  []string `json:"conflicting_signals"`
	Rating              string   `json:"rating"`
	RatingRationale     string   `json:"rating_rationale"`
}

const positionReviewSystemPrompt = `You are a financial analyst running a scheduled review of one position. You are given the symbol, a just-generated price forecast (reference price, predicted direction and price range per horizon), recent news, and — when the user actually owns the position — their quantity, average cost, and current unrealized profit or loss. Do two things: (1) refresh the qualitative deep dive — challenge the predicted ranges, and identify catalysts, risks, and supporting/conflicting signals; (2) commit to one action verdict on the scale max_sell, sell, hold, buy, max_buy. Weigh the holder's cost basis when one is given: an unrealized loss is not by itself a reason to sell, nor an unrealized gain a reason to hold. These are speculative estimates, not financial advice. Respond only by calling the emit_position_review tool.`

// GetReviewConfig returns the saved review/alert configuration, falling
// back to the table's defaults when nothing has been saved yet.
func (s *Service) GetReviewConfig(ctx context.Context) (ReviewConfig, error) {
	return s.repo.GetReviewConfig(ctx)
}

// SetReviewConfig validates and persists the review/alert configuration.
func (s *Service) SetReviewConfig(ctx context.Context, cfg ReviewConfig) error {
	if _, err := minuteOfDay(cfg.QuietStart); err != nil {
		return err
	}
	if _, err := minuteOfDay(cfg.QuietEnd); err != nil {
		return err
	}
	cfg.Timezone = strings.TrimSpace(cfg.Timezone)
	if cfg.Timezone != "" {
		if _, err := time.LoadLocation(cfg.Timezone); err != nil {
			return fmt.Errorf("%w: %q is not a known IANA timezone", ErrValidation, cfg.Timezone)
		}
	}
	return s.repo.SetReviewConfig(ctx, cfg)
}

// ListReviews returns the most recent reviews, newest first.
func (s *Service) ListReviews(ctx context.Context, limit int) ([]Review, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.repo.ListReviews(ctx, limit)
}

// quietAt reports whether t falls inside the configured night-hours
// window. A window whose start equals its end is treated as no window at
// all rather than as "always quiet", so a misconfiguration can never
// silence alerting permanently. An unparseable time or unknown timezone
// fails open for the same reason.
func (c ReviewConfig) quietAt(t time.Time) bool {
	if !c.QuietEnabled {
		return false
	}
	start, err := minuteOfDay(c.QuietStart)
	if err != nil {
		return false
	}
	end, err := minuteOfDay(c.QuietEnd)
	if err != nil || start == end {
		return false
	}
	loc := time.Local
	if c.Timezone != "" {
		l, err := time.LoadLocation(c.Timezone)
		if err != nil {
			return false
		}
		loc = l
	}
	local := t.In(loc)
	cur := local.Hour()*60 + local.Minute()
	if start < end {
		return cur >= start && cur < end
	}
	return cur >= start || cur < end // window wraps midnight, e.g. 22:00–07:00
}

// minuteOfDay parses an "HH:MM" clock time into minutes since midnight.
func minuteOfDay(hhmm string) (int, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(hhmm))
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a valid HH:MM time", ErrValidation, hhmm)
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

// normalizeRating maps model output onto the five supported ratings,
// falling back to hold for anything unrecognized — the same defensive
// stance as normalizeDirection, and the safe default for a verdict.
func normalizeRating(raw string) Rating {
	switch Rating(strings.ToLower(strings.TrimSpace(raw))) {
	case RatingMaxSell:
		return RatingMaxSell
	case RatingSell:
		return RatingSell
	case RatingBuy:
		return RatingBuy
	case RatingMaxBuy:
		return RatingMaxBuy
	default:
		return RatingHold
	}
}

// ratingRank orders ratings most-actionable-first for the digest, so the
// strongest calls in either direction lead rather than being buried in a
// list sorted alphabetically or by symbol.
func ratingRank(r Rating) int {
	switch r {
	case RatingMaxSell:
		return 0
	case RatingMaxBuy:
		return 1
	case RatingSell:
		return 2
	case RatingBuy:
		return 3
	default:
		return 4
	}
}

// RunReviewCycle runs one full pass: score any matured forecast horizons,
// then review every watched and every held symbol across all users, then
// deliver the pending digest. It returns how many symbols were reviewed.
//
// Like the forecast scheduler, this is a legitimate system actor: every
// unit of work is scoped to the owning user_id carried on the watchlist or
// transaction row itself, never an externally supplied ID.
func (s *Service) RunReviewCycle(ctx context.Context) (int, error) {
	if !s.marketData.Configured() {
		return 0, ErrMarketDataNotConfigured
	}
	if s.forecaster == nil {
		return 0, fmt.Errorf("fintech: assistant is not configured; cannot run reviews")
	}

	s.evaluateDueForecasts(ctx)

	targets, err := s.collectReviewTargets(ctx)
	if err != nil {
		return 0, err
	}
	reviewed := s.runReviews(ctx, targets)
	s.deliverPendingReviews(ctx)
	return reviewed, nil
}

// evaluateDueForecasts scores every forecast horizon whose target date has
// passed and returns how many forecasts were scored, so enabling only the
// review interval still grades past predictions. Errors are logged and
// skipped rather than aborting the pass. Both schedulers share it.
func (s *Service) evaluateDueForecasts(ctx context.Context) int {
	refs, err := s.repo.ListForecastIDsWithDueHorizons(ctx)
	if err != nil {
		log.Printf("fintech: listing due forecasts: %v", err)
		return 0
	}
	count := 0
	for _, ref := range refs {
		if ctx.Err() != nil {
			return count
		}
		if _, err := s.EvaluateForecast(ctx, ref.ForecastID); err != nil {
			log.Printf("fintech: evaluating forecast %d: %v", ref.ForecastID, err)
			continue
		}
		count++
	}
	return count
}

// collectReviewTargets builds the deduped work list: every enabled watchlist
// entry plus every open position. A symbol that is both held and watched is
// reviewed once, tagged as a holding — the position context matters more — but
// keeping the watchlist's horizons and its id, so last_forecast_at is still
// stamped and the scheduler does not immediately forecast it again.
func (s *Service) collectReviewTargets(ctx context.Context) ([]reviewTarget, error) {
	entries, err := s.repo.ListEnabledWatchlist(ctx)
	if err != nil {
		return nil, err
	}
	positions, err := s.ListHeldPositions(ctx)
	if err != nil {
		return nil, err
	}

	index := make(map[int64]int, len(entries)+len(positions))
	targets := make([]reviewTarget, 0, len(entries)+len(positions))

	for _, e := range entries {
		horizons := e.Horizons
		if len(horizons) == 0 {
			horizons = defaultReviewHorizons
		}
		index[e.InstrumentID] = len(targets)
		targets = append(targets, reviewTarget{
			instrumentID: e.InstrumentID,
			symbol:       e.Symbol,
			horizons:     horizons,
			source:       ReviewSourceWatchlist,
			watchlistID:  e.ID,
		})
	}

	// positions is not appended to below, so pointers into it stay valid.
	for i := range positions {
		p := &positions[i]
		k := p.InstrumentID
		if at, ok := index[k]; ok {
			// Held and watched: review it once, as a holding, but keep the
			// watchlist's configured horizons and ID.
			targets[at].source = ReviewSourceHolding
			targets[at].position = &p.Holding
			continue
		}
		index[k] = len(targets)
		targets = append(targets, reviewTarget{
			instrumentID: p.InstrumentID,
			symbol:       p.Symbol,
			horizons:     defaultReviewHorizons,
			source:       ReviewSourceHolding,
			position:     &p.Holding,
		})
	}
	return targets, nil
}

// runReviews reviews each target in turn, returning how many succeeded.
// One failing symbol is logged and skipped rather than stalling the pass,
// matching the forecast scheduler's posture.
func (s *Service) runReviews(ctx context.Context, targets []reviewTarget) int {
	if len(targets) > maxReviewsPerCycle {
		log.Printf("fintech review: %d targets exceeds the per-cycle cap of %d; the remainder run next cycle",
			len(targets), maxReviewsPerCycle)
		targets = targets[:maxReviewsPerCycle]
	}

	count := 0
	for i, t := range targets {
		if ctx.Err() != nil {
			return count
		}
		if i > 0 {
			select {
			case <-ctx.Done():
				return count
			case <-time.After(reviewDelay):
			}
		}
		if _, err := s.reviewTargetOnce(ctx, t); err != nil {
			log.Printf("fintech review: reviewing %s: %v", t.symbol, err)
			continue
		}
		if t.watchlistID != 0 {
			if err := s.repo.TouchWatchlistForecastedAt(ctx, t.watchlistID, time.Now()); err != nil {
				log.Printf("fintech review: stamping watchlist %d: %v", t.watchlistID, err)
			}
		}
		count++
	}
	return count
}

// reviewTargetOnce is the per-symbol unit of work: generate a fresh
// forecast, then make one structured call that both refreshes the deep
// dive and commits to a verdict, persisting the enrichment onto the
// forecast and the verdict as a review row.
func (s *Service) reviewTargetOnce(ctx context.Context, t reviewTarget) (Review, error) {
	forecast, err := s.CreateForecast(ctx, t.symbol, t.horizons, "")
	if err != nil {
		return Review{}, err
	}

	news, err := s.marketData.News(ctx, forecast.Symbol, reviewNewsLimit)
	if err != nil {
		news = nil // best-effort, as in CreateForecast
	}

	raw, usage, err := s.forecaster.CreateStructuredMessage(ctx, StructuredRequest{
		System:       positionReviewSystemPrompt,
		Prompt:       buildPositionReviewPrompt(forecast, t, news),
		OutputName:   "emit_position_review",
		OutputSchema: emitPositionReviewSchema,
	})
	if recErr := s.repo.RecordAIUsage(ctx, "review", usage); recErr != nil {
		log.Printf("fintech: failed to record review AI usage: %v", recErr)
	}
	if err != nil {
		return Review{}, fmt.Errorf("fintech: generating position review: %w", err)
	}

	var out positionReviewToolOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return Review{}, fmt.Errorf("fintech: parsing position review: %w", err)
	}

	enrichment := ForecastEnrichment{
		Summary:             out.Summary,
		PriceRangeChallenge: out.PriceRangeChallenge,
		Catalysts:           out.Catalysts,
		Risks:               out.Risks,
		SupportingSignals:   out.SupportingSignals,
		ConflictingSignals:  out.ConflictingSignals,
	}
	if err := s.repo.UpdateForecastEnrichment(ctx, forecast.ID, enrichment, time.Now()); err != nil {
		return Review{}, err
	}

	forecastID := forecast.ID
	return s.repo.InsertReview(ctx, Review{
		InstrumentID: forecast.InstrumentID,
		ForecastID:   &forecastID,
		Symbol:       forecast.Symbol,
		Source:       t.source,
		Rating:       normalizeRating(out.Rating),
		Rationale:    out.RatingRationale,
		ReviewedAt:   time.Now(),
	})
}

// deliverPendingReviews emails every review that hasn't been reported yet,
// unless alerting is off or the current time falls inside the night-hours
// window. Rows are stamped only after a successful send, so a suppressed
// night run — or a transient SMTP failure — is picked up by the next
// delivery rather than lost.
func (s *Service) deliverPendingReviews(ctx context.Context) {
	cfg, err := s.repo.GetReviewConfig(ctx)
	if err != nil {
		log.Printf("fintech review: loading review config: %v", err)
		return
	}
	if !cfg.AlertEnabled {
		return
	}
	now := time.Now()
	if cfg.quietAt(now) {
		log.Printf("fintech review: inside night hours (%s–%s), holding the digest until the next cycle",
			cfg.QuietStart, cfg.QuietEnd)
		return
	}
	if s.alerter == nil || !s.alerter.Deliverable(ctx) {
		return
	}

	pending, err := s.repo.ListUnreportedReviews(ctx)
	if err != nil {
		log.Printf("fintech review: listing unreported reviews: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}

	subject, body := composeReviewDigest(pending, now)
	if err := s.alerter.Send(ctx, subject, body); err != nil {
		log.Printf("fintech review: sending digest: %v", err)
		return
	}

	ids := make([]int64, len(pending))
	for i, rev := range pending {
		ids[i] = rev.ID
	}
	if err := s.repo.MarkReviewsReported(ctx, ids, now); err != nil {
		log.Printf("fintech review: marking reviews reported: %v", err)
	}
}

// composeReviewDigest renders the pending reviews as a plaintext email,
// most-actionable verdict first. Note that this goes to twire's single
// global recipient list, so on a multi-user install every user's positions
// share one digest — an accepted consequence of reusing that config.
func composeReviewDigest(reviews []Review, now time.Time) (subject, body string) {
	ordered := make([]Review, len(reviews))
	copy(ordered, reviews)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ri, rj := ratingRank(ordered[i].Rating), ratingRank(ordered[j].Rating); ri != rj {
			return ri < rj
		}
		return ordered[i].Symbol < ordered[j].Symbol
	})

	counts := make(map[Rating]int, 5)
	for _, rev := range ordered {
		counts[rev.Rating]++
	}
	headline := make([]string, 0, 5)
	for _, r := range []Rating{RatingMaxSell, RatingSell, RatingHold, RatingBuy, RatingMaxBuy} {
		if counts[r] > 0 {
			headline = append(headline, fmt.Sprintf("%d %s", counts[r], ratingLabel(r)))
		}
	}
	subject = fmt.Sprintf("[morpheus] Position review — %s", strings.Join(headline, ", "))

	var b strings.Builder
	fmt.Fprintf(&b, "Scheduled position review, %s.\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "%d symbol(s) reviewed since the last digest.\n\n", len(ordered))
	for _, rev := range ordered {
		held := "watched"
		if rev.Source == ReviewSourceHolding {
			held = "held"
		}
		fmt.Fprintf(&b, "%-8s %-9s (%s, reviewed %s)\n",
			rev.Symbol, ratingLabel(rev.Rating), held, rev.ReviewedAt.Format("2006-01-02 15:04"))
		if rev.Rationale != "" {
			fmt.Fprintf(&b, "    %s\n", rev.Rationale)
		}
		b.WriteString("\n")
	}
	b.WriteString("These are speculative model estimates, not financial advice.\n")
	return subject, b.String()
}

// ratingLabel renders a Rating for human display.
func ratingLabel(r Rating) string {
	switch r {
	case RatingMaxSell:
		return "MAX SELL"
	case RatingSell:
		return "SELL"
	case RatingBuy:
		return "BUY"
	case RatingMaxBuy:
		return "MAX BUY"
	default:
		return "HOLD"
	}
}

func buildPositionReviewPrompt(f Forecast, t reviewTarget, news []NewsItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Scheduled review of %s.\n", f.Symbol)
	fmt.Fprintf(&b, "Reference price: %d cents (as of %s).\n", f.ReferencePriceCents, f.RequestedAt.Format(time.RFC3339))

	if t.position != nil {
		unrealized := unrealizedAtPrice(*t.position, f.ReferencePriceCents)
		fmt.Fprintf(&b, "The user HOLDS this position: %s units, average cost %s per unit, total cost %s.\n",
			t.position.Quantity, formatCents(t.position.AvgCostCents), formatCents(t.position.TotalCostCents))
		fmt.Fprintf(&b, "At the reference price that is an unrealized %s of %s.\n",
			gainOrLoss(unrealized), formatCents(abs64(unrealized)))
	} else {
		b.WriteString("The user does NOT hold this position; it is on their watchlist.\n")
	}

	if f.Rationale != "" {
		fmt.Fprintf(&b, "Forecast rationale: %s\n", f.Rationale)
	}
	b.WriteString("Forecast horizons (direction, price range, confidence):\n")
	for _, h := range f.Horizons {
		fmt.Fprintf(&b, "- %dd: %s, %s–%s, confidence %.0f%%\n",
			h.HorizonDays, h.PredictedDirection,
			formatCents(h.PredictedLowCents), formatCents(h.PredictedHighCents), h.Confidence*100)
	}

	if len(news) == 0 {
		b.WriteString("No recent news was available.\n")
	} else {
		b.WriteString("Recent news headlines:\n")
		for _, n := range news {
			fmt.Fprintf(&b, "- %s (%s, %s)\n", n.Headline, n.Source, n.PublishedAt.Format("2006-01-02"))
		}
	}
	b.WriteString("\nRefresh the deep dive, then commit to one rating of max_sell, sell, hold, buy, or max_buy.")
	return b.String()
}

// unrealizedAtPrice values a position at priceCents per unit and returns
// the unrealized profit or loss in cents, using the same big.Rat
// arithmetic as GetPortfolioSummary.
func unrealizedAtPrice(h Holding, priceCents int64) int64 {
	qty, ok := new(big.Rat).SetString(h.Quantity)
	if !ok {
		return 0
	}
	return ratToCents(new(big.Rat).Mul(qty, big.NewRat(priceCents, 1))) - h.TotalCostCents
}

func gainOrLoss(cents int64) string {
	if cents < 0 {
		return "loss"
	}
	return "gain"
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func formatCents(cents int64) string {
	return fmt.Sprintf("€%.2f", float64(cents)/100)
}
