package twire

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// alertCooldown bounds how often the same (canary, source IP) pair triggers
// an email, so a port scanner hammering a canary can't flood the inbox.
const alertCooldown = 5 * time.Minute

// Service manages the canary listeners, records hits, and sends rate-limited
// email alerts. Listeners are opt-in and tracked in-memory; enabled-state,
// events, and alert config are persisted via the repository.
type Service struct {
	repo        *Repository
	log         *slog.Logger
	profiles    []ServiceProfile
	byKey       map[string]ServiceProfile
	encKey      []byte
	envDefaults AlertConfig

	mu          sync.Mutex
	listeners   map[string]net.Listener
	bindErrs    map[string]string
	customByKey map[string]ServiceProfile // operator-defined canaries, loaded from the DB

	alertMu   sync.Mutex
	lastAlert map[string]time.Time
}

// NewService builds a Service. secret is WINTERMUTE_SECRET, used (via
// deriveEncryptionKey) to encrypt the stored SMTP password; empty means no
// password can be saved, which SetAlertConfig reports as ErrNoSecret rather
// than working around. envDefaults supplies alert settings from environment
// variables, used until a configuration is saved in-app.
func NewService(repo *Repository, secret []byte, envDefaults AlertConfig, log *slog.Logger) *Service {
	return &Service{
		repo:        repo,
		log:         log,
		profiles:    catalog,
		byKey:       catalogByKey(),
		encKey:      deriveEncryptionKey(secret),
		envDefaults: envDefaults,
		listeners:   make(map[string]net.Listener),
		bindErrs:    make(map[string]string),
		customByKey: make(map[string]ServiceProfile),
		lastAlert:   make(map[string]time.Time),
	}
}

// SecretConfigured reports whether a key is available to encrypt a stored SMTP
// password. The API surfaces this so the settings form can say why saving a
// password is refused, instead of the refusal arriving as a bare error on
// submit.
func (s *Service) SecretConfigured() bool { return len(s.encKey) > 0 }

// Start launches listeners for every enabled canary and tears them all down
// when ctx is cancelled. Safe to call once after construction.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	// Load operator-defined canaries first so enabled ones below can bind.
	// A failure here is logged but doesn't stop the built-in canaries.
	if customs, err := s.repo.ListCustomCanaries(ctx); err != nil {
		s.log.Warn("twire: loading custom canaries", "error", err)
	} else {
		for _, p := range customs {
			s.customByKey[p.Key] = p
		}
	}
	enabled, err := s.repo.EnabledSet(ctx)
	if err != nil {
		s.log.Warn("twire: loading enabled canaries", "error", err)
		s.mu.Unlock()
		return
	}
	for key, on := range enabled {
		if on {
			s.startListenerLocked(key)
		}
	}
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		s.closeAll()
	}()
}

// Status returns the live status of every canary, built-in then custom.
func (s *Service) Status(ctx context.Context) ([]CanaryStatus, error) {
	enabled, err := s.repo.EnabledSet(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := s.repo.EventCountsByProfile(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CanaryStatus, 0, len(s.profiles)+len(s.customByKey))
	add := func(p ServiceProfile, custom bool) {
		_, listening := s.listeners[p.Key]
		bindErr := s.bindErrs[p.Key]
		privRequired := p.Port < 1024 && !listening &&
			strings.Contains(strings.ToLower(bindErr), "permission denied")
		out = append(out, CanaryStatus{
			ServiceProfile:    p,
			Enabled:           enabled[p.Key],
			Listening:         listening,
			LastError:         bindErr,
			HitCount:          counts[p.Key],
			Custom:            custom,
			PrivilegeRequired: privRequired,
		})
	}
	for _, p := range s.profiles {
		add(p, false)
	}
	// Custom canaries follow the built-in catalog, ordered by port for a
	// stable listing.
	customs := make([]ServiceProfile, 0, len(s.customByKey))
	for _, p := range s.customByKey {
		customs = append(customs, p)
	}
	sort.Slice(customs, func(i, j int) bool { return customs[i].Port < customs[j].Port })
	for _, p := range customs {
		add(p, true)
	}
	return out, nil
}

// lookup returns the profile for key from the built-in catalog or the custom
// set, plus whether it is custom. It takes s.mu to read customByKey safely.
func (s *Service) lookup(key string) (ServiceProfile, bool, bool) {
	if p, ok := s.byKey[key]; ok {
		return p, true, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.customByKey[key]
	return p, ok, ok
}

// Enable turns a canary on (persisted) and starts its listener. A bind
// failure is not an error here — it is recorded and surfaced via Status —
// so enabling a port already in use never fails the request.
func (s *Service) Enable(ctx context.Context, key string) error {
	if _, ok, _ := s.lookup(key); !ok {
		return ErrUnknownCanary
	}
	if err := s.repo.SetCanaryEnabled(ctx, key, true); err != nil {
		return err
	}
	s.mu.Lock()
	s.startListenerLocked(key)
	s.mu.Unlock()
	return nil
}

// Disable turns a canary off (persisted) and stops its listener.
func (s *Service) Disable(ctx context.Context, key string) error {
	if _, ok, _ := s.lookup(key); !ok {
		return ErrUnknownCanary
	}
	if err := s.repo.SetCanaryEnabled(ctx, key, false); err != nil {
		return err
	}
	s.mu.Lock()
	if ln, ok := s.listeners[key]; ok {
		ln.Close()
		delete(s.listeners, key)
	}
	delete(s.bindErrs, key)
	s.mu.Unlock()
	return nil
}

// CreateCustom validates and persists an operator-defined canary, then makes
// it available (disabled by default — the operator enables it afterwards).
// Returns ErrValidation for bad input, ErrPortReserved when the port belongs
// to a built-in canary, or ErrPortTaken when another custom canary already
// uses it.
func (s *Service) CreateCustom(ctx context.Context, name string, port int, description, banner string) (CanaryStatus, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return CanaryStatus{}, ErrValidation
	}
	if port < 1 || port > 65535 {
		return CanaryStatus{}, ErrValidation
	}
	for _, p := range s.profiles {
		if p.Port == port {
			return CanaryStatus{}, ErrPortReserved
		}
	}

	profile := ServiceProfile{
		Key:         customProfileKey(port),
		Name:        name,
		Port:        port,
		Description: strings.TrimSpace(description),
		Banner:      banner,
	}
	if err := s.repo.InsertCustomCanary(ctx, profile); err != nil {
		return CanaryStatus{}, err
	}
	s.mu.Lock()
	s.customByKey[profile.Key] = profile
	s.mu.Unlock()

	return CanaryStatus{ServiceProfile: profile, Custom: true}, nil
}

// DeleteCustom stops and removes an operator-defined canary. Built-in
// canaries return ErrNotCustom; unknown keys return ErrUnknownCanary.
func (s *Service) DeleteCustom(ctx context.Context, key string) error {
	if _, ok := s.byKey[key]; ok {
		return ErrNotCustom
	}
	found, err := s.repo.DeleteCustomCanary(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		return ErrUnknownCanary
	}
	s.mu.Lock()
	if ln, ok := s.listeners[key]; ok {
		ln.Close()
		delete(s.listeners, key)
	}
	delete(s.bindErrs, key)
	delete(s.customByKey, key)
	s.mu.Unlock()
	return nil
}

// ListEvents returns the most recent recorded connection attempts.
func (s *Service) ListEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.repo.ListEvents(ctx, limit)
}

// GetAlertConfig returns the effective alert configuration: the saved in-app
// config if present, otherwise the environment-derived defaults.
//
// A stored password that cannot be decrypted — WINTERMUTE_SECRET unset or
// rotated since it was saved — costs the config its password rather than
// failing the call: the rest of the configuration is still worth reading, and
// the settings page's job is to say a password needs re-entering.
func (s *Service) GetAlertConfig(ctx context.Context) (AlertConfig, error) {
	cfg, enc, nonce, found, err := s.repo.GetAlertConfig(ctx)
	if err != nil {
		return AlertConfig{}, err
	}
	if !found {
		return s.envDefaults, nil
	}
	if len(enc) > 0 {
		if !s.SecretConfigured() {
			s.log.Warn("twire: a stored SMTP password cannot be read back without WINTERMUTE_SECRET")
			return cfg, nil
		}
		pw, err := decryptSecret(s.encKey, enc, nonce)
		if err != nil {
			s.log.Warn("twire: stored SMTP password could not be decrypted; re-enter it",
				"error", err)
			return cfg, nil
		}
		cfg.SMTPPassword = pw
	}
	return cfg, nil
}

// SetAlertConfig persists a new alert configuration. When passwordSupplied is
// false the previously stored SMTP password is retained, so editing other
// fields doesn't require re-entering it.
//
// Supplying a password with no WINTERMUTE_SECRET set is ErrNoSecret. Every
// other field still saves in that state, so alerting can be configured up to
// the credential and the operator is told exactly what is missing.
func (s *Service) SetAlertConfig(ctx context.Context, cfg AlertConfig, passwordSupplied bool) error {
	var enc, nonce []byte
	if !passwordSupplied {
		if _, e, n, found, err := s.repo.GetAlertConfig(ctx); err != nil {
			return err
		} else if found {
			enc, nonce = e, n
		}
	}
	if enc == nil && cfg.SMTPPassword != "" {
		if !s.SecretConfigured() {
			return ErrNoSecret
		}
		var err error
		if enc, nonce, err = encryptSecret(s.encKey, cfg.SMTPPassword); err != nil {
			return err
		}
	}
	return s.repo.SetAlertConfig(ctx, cfg, enc, nonce)
}

// SendTestAlert sends a test email with the effective configuration,
// returning a validation error if alerting isn't deliverable.
func (s *Service) SendTestAlert(ctx context.Context) error {
	cfg, err := s.GetAlertConfig(ctx)
	if err != nil {
		return err
	}
	if !cfg.deliverable() {
		return ErrValidation
	}
	return sendEmail(cfg, "[twire] test alert",
		"This is a test alert from wintermute twire. If you received this, email alerting is configured correctly.\n")
}

// AlertDeliverable reports whether an alert could actually be sent with the
// effective configuration. It lets other modules skip composing a message that
// could not be delivered.
func (s *Service) AlertDeliverable(ctx context.Context) bool {
	cfg, err := s.GetAlertConfig(ctx)
	if err != nil {
		return false
	}
	return cfg.deliverable()
}

// SendAlert delivers an arbitrary plaintext alert using the effective
// configuration. It exists so other modules can reuse twire's single SMTP
// setup and recipient list instead of configuring email twice; twire's own
// canary alerts go through sendAlert.
//
// This is what the fintech package's review digest wanted and did not have —
// see the nil alerter in app.buildFintech, and the note there.
func (s *Service) SendAlert(ctx context.Context, subject, body string) error {
	cfg, err := s.GetAlertConfig(ctx)
	if err != nil {
		return err
	}
	if !cfg.deliverable() {
		return ErrValidation
	}
	return sendEmail(cfg, subject, body)
}

// recordHit persists an event and fires a rate-limited alert. Called from
// the connection handler; never blocks on email (that runs in a goroutine).
func (s *Service) recordHit(profile ServiceProfile, remoteIP string, remotePort int, preview string) {
	ev := Event{
		ProfileKey:  profile.Key,
		ServiceName: profile.Name,
		Port:        profile.Port,
		RemoteIP:    remoteIP,
		RemotePort:  remotePort,
		DataPreview: preview,
		OccurredAt:  time.Now(),
	}
	if _, err := s.repo.InsertEvent(context.Background(), ev); err != nil {
		s.log.Error("twire: recording event", "canary", profile.Key, "error", err)
	}
	if s.shouldAlert(profile.Key, remoteIP) {
		go s.sendAlert(profile, remoteIP, remotePort, preview)
	}
}

// shouldAlert reports whether an alert for (key, ip) is outside the cooldown.
//
// Entries past the cooldown are swept on the way through. Without that the map
// grows one entry per distinct source IP and never shrinks — and the source IP
// of a canary hit is chosen by whoever is scanning the network, so a sweep
// across a wide range would grow it without limit.
func (s *Service) shouldAlert(key, ip string) bool {
	s.alertMu.Lock()
	defer s.alertMu.Unlock()
	id := key + "|" + ip
	now := time.Now()
	if last, ok := s.lastAlert[id]; ok && now.Sub(last) < alertCooldown {
		return false
	}
	s.lastAlert[id] = now
	for k, last := range s.lastAlert {
		if now.Sub(last) >= alertCooldown {
			delete(s.lastAlert, k)
		}
	}
	return true
}

func (s *Service) sendAlert(profile ServiceProfile, ip string, port int, preview string) {
	cfg, err := s.GetAlertConfig(context.Background())
	if err != nil {
		s.log.Error("twire: loading alert config for alert", "error", err)
		return
	}
	if !cfg.deliverable() {
		return
	}
	subject := "[twire] " + profile.Name + " canary tripped from " + ip
	body := "A connection was made to a wintermute twire canary.\n\n" +
		"Service:      " + profile.Name + "\n" +
		"Canary port:  " + strconv.Itoa(profile.Port) + "\n" +
		"Source:       " + ip + ":" + strconv.Itoa(port) + "\n" +
		"Time:         " + time.Now().Format(time.RFC1123Z) + "\n" +
		"Data preview: " + preview + "\n\n" +
		"Nothing legitimate should connect to this port — treat this as a probe of your network.\n"
	if err := sendEmail(cfg, subject, body); err != nil {
		s.log.Error("twire: sending alert email", "canary", profile.Key, "error", err)
	}
}

func (s *Service) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, ln := range s.listeners {
		ln.Close()
		delete(s.listeners, key)
	}
}
