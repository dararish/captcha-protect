package captcha_protect

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/dararish/captcha-protect/internal/filelock"
	"github.com/dararish/captcha-protect/internal/helper"
	plog "github.com/dararish/captcha-protect/internal/log"
	"github.com/dararish/captcha-protect/internal/state"

	lru "github.com/patrickmn/go-cache"
)

var (
	log *slog.Logger
)

type Config struct {
	RateLimit             uint     `json:"rateLimit"`
	Window                int64    `json:"window"`
	IPv4SubnetMask        int      `json:"ipv4subnetMask"`
	IPv6SubnetMask        int      `json:"ipv6subnetMask"`
	IPForwardedHeader     string   `json:"ipForwardedHeader"`
	IPDepth               int      `json:"ipDepth"`
	ProtectParameters     string   `json:"protectParameters"`
	ProtectRoutes         []string `json:"protectRoutes"`
	ExcludeRoutes         []string `json:"excludeRoutes"`
	ProtectFileExtensions []string `json:"protectFileExtensions"`
	ProtectHttpMethods    []string `json:"protectHttpMethods"`
	GoodBots              []string `json:"goodBots"`
	ExemptIPs             []string `json:"exemptIps"`
	ExemptUserAgents      []string `json:"exemptUserAgents"`
	ChallengeURL          string   `json:"challengeURL"`
	ChallengeTmpl         string   `json:"challengeTmpl"`
	ChallengeStatusCode   int      `json:"challengeStatusCode"`
	CaptchaProvider       string   `json:"captchaProvider"`
	SiteKey               string   `json:"siteKey"`
	SecretKey             string   `json:"secretKey"`
	EnableStatsPage       string   `json:"enableStatsPage"`
	LogLevel              string   `json:"loglevel,omitempty"`
	PersistentStateFile   string   `json:"persistentStateFile"`
	Mode                  string   `json:"mode"`
}

type CaptchaProtect struct {
	next               http.Handler
	name               string
	config             *Config
	rateCache          *lru.Cache
	verifiedCache      *lru.Cache
	botCache           *lru.Cache
	captchaConfig      CaptchaConfig
	exemptIps          []*net.IPNet
	tmpl               *template.Template
	ipv4Mask           net.IPMask
	ipv6Mask           net.IPMask
	protectRoutesRegex []*regexp.Regexp
	excludeRoutesRegex []*regexp.Regexp
	stateMutex         sync.RWMutex
	stateChanged       chan struct{}
	lastStateReload    time.Time
}

type CaptchaConfig struct {
	js       string
	key      string
	validate string
}

type captchaResponse struct {
	Success bool `json:"success"`
}

func CreateConfig() *Config {
	return &Config{
		RateLimit:             20,
		Window:                86400,
		IPv4SubnetMask:        16,
		IPv6SubnetMask:        64,
		IPForwardedHeader:     "",
		ProtectParameters:     "false",
		ProtectRoutes:         []string{},
		ExcludeRoutes:         []string{},
		ProtectHttpMethods:    []string{},
		ProtectFileExtensions: []string{},
		GoodBots:              []string{},
		ExemptIPs:             []string{},
		ExemptUserAgents:      []string{},
		ChallengeURL:          "/challenge",
		ChallengeTmpl:         "challenge.tmpl.html",
		ChallengeStatusCode:   0,
		EnableStatsPage:       "false",
		LogLevel:              "INFO",
		IPDepth:               0,
		CaptchaProvider:       "turnstile",
		Mode:                  "prefix",
	}
}

func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	return NewCaptchaProtect(ctx, next, config, name)
}

func NewCaptchaProtect(ctx context.Context, next http.Handler, config *Config, name string) (*CaptchaProtect, error) {
	log = plog.New(config.LogLevel)

	expiration := time.Duration(config.Window) * time.Second
	log.Debug("Captcha config", "config", config)

	if len(config.ProtectRoutes) == 0 && config.Mode != "suffix" {
		return nil, fmt.Errorf("you must protect at least one route with the protectRoutes config value. / will cover your entire site")
	}

	protectRoutesRegex := []*regexp.Regexp{}
	excludeRoutesRegex := []*regexp.Regexp{}
	if config.Mode == "regex" {
		for _, r := range config.ProtectRoutes {
			cr, err := regexp.Compile(r)
			if err != nil {
				return nil, fmt.Errorf("invalid regex in protectRoutes: %s", r)
			}
			protectRoutesRegex = append(protectRoutesRegex, cr)
		}
		for _, r := range config.ExcludeRoutes {
			cr, err := regexp.Compile(r)
			if err != nil {
				return nil, fmt.Errorf("invalid regex in excludeRoutes: %s", r)
			}
			excludeRoutesRegex = append(excludeRoutesRegex, cr)
		}
	} else if config.Mode != "prefix" && config.Mode != "suffix" {
		return nil, fmt.Errorf("unknown mode: %s. Supported values are prefix, suffix, and regex", config.Mode)
	}

	// put exempt user agents in lowercase for quicker comparisons
	ua := []string{}
	for _, a := range config.ExemptUserAgents {
		ua = append(ua, strings.ToLower(a))
	}
	config.ExemptUserAgents = ua

	if config.ChallengeURL == "/" {
		return nil, fmt.Errorf("your challenge URL can not be the entire site. Default is `/challenge`. A blank value will have challenges presented on the visit that trips the rate limit")
	}

	// when challenging on the same page that tripped the rate limiter
	// add a url parameter to detect on
	if config.ChallengeURL == "" {
		config.ChallengeURL = "?challenge=true"
	}

	if len(config.ProtectHttpMethods) == 0 {
		config.ProtectHttpMethods = []string{
			"GET",
			"HEAD",
		}
	}
	config.ParseHttpMethods()

	var tmpl *template.Template
	if _, err := os.Stat(config.ChallengeTmpl); os.IsNotExist(err) {
		log.Warn("Unable to find template file. Using default template.", "challengeTmpl", config.ChallengeTmpl)
		ts := helper.GetDefaultTmpl()
		tmpl, err = template.New("challenge").Parse(ts)
		if err != nil {
			return nil, fmt.Errorf("unable to parse challenge template: %v", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("error checking for template file %s: %v", config.ChallengeTmpl, err)
	} else {
		tmpl, err = template.ParseFiles(config.ChallengeTmpl)
		if err != nil {
			return nil, fmt.Errorf("unable to parse challenge template file %s: %v", config.ChallengeTmpl, err)
		}
	}

	if !slices.Contains(config.ProtectFileExtensions, "html") {
		config.ProtectFileExtensions = append(config.ProtectFileExtensions, "html")
	}

	// transform exempt IP strings into what go can easily parse (net.IPNet)
	var ips []*net.IPNet
	exemptIps := []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"fc00::/8",
	}
	exemptIps = append(exemptIps, config.ExemptIPs...)
	for _, ip := range exemptIps {
		parsedIp, err := helper.ParseCIDR(ip)
		if err != nil {
			return nil, fmt.Errorf("error parsing cidr %s: %v", ip, err)
		}
		ips = append(ips, parsedIp)
	}

	bc := CaptchaProtect{
		next:               next,
		name:               name,
		config:             config,
		rateCache:          lru.New(expiration, 1*time.Minute),
		botCache:           lru.New(expiration, 1*time.Hour),
		verifiedCache:      lru.New(expiration, 1*time.Hour),
		exemptIps:          ips,
		tmpl:               tmpl,
		protectRoutesRegex: protectRoutesRegex,
		excludeRoutesRegex: excludeRoutesRegex,
	}

	// if a status code was not configured
	// retain the default set before this config option was added
	if config.ChallengeStatusCode == 0 {
		bc.config.ChallengeStatusCode = http.StatusOK
		if bc.ChallengeOnPage() {
			bc.config.ChallengeStatusCode = http.StatusTooManyRequests
		}
	}

	err := bc.SetIpv4Mask(config.IPv4SubnetMask)
	if err != nil {
		return nil, err
	}

	err = bc.SetIpv6Mask(config.IPv6SubnetMask)
	if err != nil {
		return nil, err
	}

	// set the captcha config based on the provider
	// thanks to https://github.com/maxlerebourg/crowdsec-bouncer-traefik-plugin/blob/4708d76854c7ae95fa7313c46fbe21959be2fff1/pkg/captcha/captcha.go#L39-L55
	// for the struct/idea
	switch config.CaptchaProvider {
	case "hcaptcha":
		bc.captchaConfig = CaptchaConfig{
			js:       "https://hcaptcha.com/1/api.js",
			key:      "h-captcha",
			validate: "https://api.hcaptcha.com/siteverify",
		}
	case "recaptcha":
		bc.captchaConfig = CaptchaConfig{
			js:       "https://www.google.com/recaptcha/api.js",
			key:      "g-recaptcha",
			validate: "https://www.google.com/recaptcha/api/siteverify",
		}
	case "turnstile":
		bc.captchaConfig = CaptchaConfig{
			js:       "https://challenges.cloudflare.com/turnstile/v0/api.js",
			key:      "cf-turnstile",
			validate: "https://challenges.cloudflare.com/turnstile/v0/siteverify",
		}
	default:
		return nil, fmt.Errorf("invalid captcha provider: %s", config.CaptchaProvider)
	}

	if config.PersistentStateFile != "" {
		bc.stateChanged = make(chan struct{}, 1)
		bc.loadState()
		childCtx, cancel := context.WithCancel(ctx)
		go bc.saveStateOnChange(childCtx)
		go func() {
			<-ctx.Done()
			log.Debug("Context canceled, calling child cancel...")
			cancel()
		}()
	}

	return &bc, nil
}

func (bc *CaptchaProtect) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Reload state from file every 5 seconds if persistent state is enabled
	if bc.config.PersistentStateFile != "" {
		bc.reloadStateIfNeeded()
	}

	clientIP, ipRange := bc.getClientIP(req)
	challengeOnPage := bc.ChallengeOnPage()
	if challengeOnPage && req.Method == http.MethodPost {
		if req.URL.Query().Get("challenge") != "" {
			statusCode := bc.verifyChallengePage(rw, req, clientIP)
			log.Info("Captcha challenge", "clientIP", clientIP, "method", req.Method, "path", req.URL.Path, "status", statusCode, "useragent", req.UserAgent())
			return
		}
	} else if req.URL.Path == bc.config.ChallengeURL {
		switch req.Method {
		case http.MethodGet:
			destination := req.URL.Query().Get("destination")
			log.Info("Captcha challenge", "clientIP", clientIP, "method", req.Method, "path", req.URL.Path, "destination", destination, "useragent", req.UserAgent())
			bc.serveChallengePage(rw, destination)
		case http.MethodPost:
			statusCode := bc.verifyChallengePage(rw, req, clientIP)
			log.Info("Captcha challenge", "clientIP", clientIP, "method", req.Method, "path", req.URL.Path, "status", statusCode, "useragent", req.UserAgent())
		default:
			http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	} else if req.URL.Path == "/captcha-protect/stats" && bc.config.EnableStatsPage == "true" {
		log.Info("Captcha stats", "clientIP", clientIP, "method", req.Method, "path", req.URL.Path, "useragent", req.UserAgent())
		bc.serveStatsPage(rw, clientIP)
		return
	}

	if !bc.shouldApply(req, clientIP) {
		bc.next.ServeHTTP(rw, req)
		return
	}
	bc.registerRequest(ipRange)

	if !bc.trippedRateLimit(ipRange) {
		bc.next.ServeHTTP(rw, req)
		return
	}

	encodedURI := url.QueryEscape(req.RequestURI)
	if bc.ChallengeOnPage() {
		log.Info("Captcha challenge", "clientIP", clientIP, "method", req.Method, "path", req.URL.Path, "useragent", req.UserAgent())
		bc.serveChallengePage(rw, encodedURI)
		return
	}
	url := fmt.Sprintf("%s?destination=%s", bc.config.ChallengeURL, encodedURI)
	http.Redirect(rw, req, url, http.StatusFound)
}

func (bc *CaptchaProtect) serveChallengePage(rw http.ResponseWriter, destination string) {
	d := map[string]string{
		"SiteKey":      bc.config.SiteKey,
		"FrontendJS":   bc.captchaConfig.js,
		"FrontendKey":  bc.captchaConfig.key,
		"ChallengeURL": bc.config.ChallengeURL,
		"Destination":  destination,
	}

	// have to write http status before executing the template
	// otherwise a 200 will get served by the template execution
	rw.WriteHeader(bc.config.ChallengeStatusCode)

	err := bc.tmpl.Execute(rw, d)
	if err != nil {
		log.Error("Unable to execute go template", "tmpl", bc.config.ChallengeTmpl, "err", err)
		http.Error(rw, "Internal error", http.StatusInternalServerError)
	}
}

func (bc *CaptchaProtect) verifyChallengePage(rw http.ResponseWriter, req *http.Request, ip string) int {
	response := req.FormValue(bc.captchaConfig.key + "-response")
	if response == "" {
		http.Error(rw, "Bad request", http.StatusBadRequest)
		return http.StatusBadRequest
	}

	var body = url.Values{}
	body.Add("secret", bc.config.SecretKey)
	body.Add("response", response)
	resp, err := http.PostForm(bc.captchaConfig.validate, body)
	if err != nil {
		log.Error("Unable to validate captcha", "url", bc.captchaConfig.validate, "body", body, "err", err)
		http.Error(rw, "Internal error", http.StatusInternalServerError)
		return http.StatusInternalServerError
	}
	defer resp.Body.Close()

	var captchaResponse captchaResponse
	err = json.NewDecoder(resp.Body).Decode(&captchaResponse)
	if err != nil {
		log.Error("Unable to unmarshal captcha response", "url", bc.captchaConfig.validate, "err", err)
		http.Error(rw, "Internal error", http.StatusInternalServerError)
		return http.StatusInternalServerError
	}
	if captchaResponse.Success {
		bc.verifiedCache.Set(ip, true, lru.DefaultExpiration)
		bc.notifyStateChange()
		destination := req.FormValue("destination")
		if destination == "" {
			destination = "%2F"
		}
		u, err := url.QueryUnescape(destination)
		if err != nil {
			log.Error("Unable to unescape destination", "destination", destination, "err", err)
			u = "/"
		}
		http.Redirect(rw, req, u, http.StatusFound)
		return http.StatusFound
	}

	http.Error(rw, "Validation failed", http.StatusForbidden)

	return http.StatusForbidden
}

func (bc *CaptchaProtect) serveStatsPage(rw http.ResponseWriter, ip string) {
	// only allow excluded IPs from viewing
	if !helper.IsIpExcluded(ip, bc.exemptIps) {
		http.Error(rw, "Forbidden", http.StatusForbidden)
		return
	}

	state := state.GetState(bc.rateCache.Items(), bc.botCache.Items(), bc.verifiedCache.Items())
	jsonData, err := json.Marshal(state)
	if err != nil {
		log.Error("failed to marshal JSON", "err", err)
		http.Error(rw, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	rw.WriteHeader(http.StatusOK)
	rw.Header().Set("Content-Type", "application/json")
	_, err = rw.Write(jsonData)
	if err != nil {
		log.Error("failed to write JSON on stats reques", "err", err)
		http.Error(rw, "Internal Server Error", http.StatusInternalServerError)
		return
	}

}

func (bc *CaptchaProtect) shouldApply(req *http.Request, clientIP string) bool {
	if !slices.Contains(bc.config.ProtectHttpMethods, req.Method) {
		return false
	}

	_, verified := bc.verifiedCache.Get(clientIP)
	if verified {
		return false
	}

	if helper.IsIpExcluded(clientIP, bc.exemptIps) {
		return false
	}

	if bc.isGoodBot(req, clientIP) {
		return false
	}

	if bc.isGoodUserAgent(req.UserAgent()) {
		return false
	}

	if bc.config.Mode == "regex" {
		return bc.RouteIsProtectedRegex(req.URL.Path)
	}

	if bc.config.Mode == "suffix" {
		return bc.RouteIsProtectedSuffix(req.URL.Path)
	}

	return bc.RouteIsProtectedPrefix(req.URL.Path)
}

func (bc *CaptchaProtect) RouteIsProtectedPrefix(path string) bool {
protected:
	for _, route := range bc.config.ProtectRoutes {
		if !strings.HasPrefix(path, route) {
			continue
		}

		// we're on a protected route - make sure this route doesn't have an exclusion
		for _, eRoute := range bc.config.ExcludeRoutes {
			if strings.HasPrefix(path, eRoute) {
				continue protected
			}
		}

		// if this path isn't a file, go ahead and mark this path as protected
		ext := filepath.Ext(path)
		ext = strings.TrimPrefix(ext, ".")
		if ext == "" {
			return true
		}

		// if we have a file extension, see if we should protect this file extension type
		for _, protectedExtensions := range bc.config.ProtectFileExtensions {
			if strings.EqualFold(ext, protectedExtensions) {
				return true
			}
		}
	}

	return false
}

func (bc *CaptchaProtect) RouteIsProtectedSuffix(path string) bool {
protected:
	for _, route := range bc.config.ProtectRoutes {
		cleanPath := path
		ext := filepath.Ext(path)
		if ext != "" {
			cleanPath = strings.TrimSuffix(path, ext)
		}
		if !strings.HasSuffix(cleanPath, route) {
			continue
		}

		// we're on a protected route - make sure this route doesn't have an exclusion
		for _, eRoute := range bc.config.ExcludeRoutes {
			if strings.HasPrefix(cleanPath, eRoute) {
				continue protected
			}
		}

		// if this path isn't a file, go ahead and mark this path as protected
		ext = strings.TrimPrefix(ext, ".")
		if ext == "" {
			return true
		}

		// if we have a file extension, see if we should protect this file extension type
		for _, protectedExtensions := range bc.config.ProtectFileExtensions {
			if strings.EqualFold(ext, protectedExtensions) {
				return true
			}
		}
	}

	return false
}

func (bc *CaptchaProtect) isGoodUserAgent(ua string) bool {
	ua = strings.ToLower(ua)
	for _, agentPrefix := range bc.config.ExemptUserAgents {
		if strings.HasPrefix(ua, agentPrefix) {
			return true
		}
	}

	return false
}

func (bc *CaptchaProtect) RouteIsProtectedRegex(path string) bool {
protected:
	for _, routeRegex := range bc.protectRoutesRegex {
		matched := routeRegex.MatchString(path)
		if !matched {
			continue
		}

		for _, excludeRegex := range bc.excludeRoutesRegex {
			excluded := excludeRegex.MatchString(path)
			if excluded {
				continue protected
			}
		}

		ext := filepath.Ext(path)
		ext = strings.TrimPrefix(ext, ".")
		if ext == "" {
			return true
		}

		for _, protectedExtension := range bc.config.ProtectFileExtensions {
			if strings.EqualFold(ext, protectedExtension) {
				return true
			}
		}
	}

	return false
}

func (bc *CaptchaProtect) trippedRateLimit(ip string) bool {
	v, ok := bc.rateCache.Get(ip)
	if !ok {
		log.Error("IP not found, but should already be set", "ip", ip)
		return false
	}
	return v.(uint) > bc.config.RateLimit
}

func (bc *CaptchaProtect) registerRequest(ip string) {
	err := bc.rateCache.Add(ip, uint(1), lru.DefaultExpiration)
	if err == nil {
		bc.notifyStateChange()
		return
	}

	_, err = bc.rateCache.IncrementUint(ip, uint(1))
	if err != nil {
		log.Error("Unable to set rate cache", "ip", ip)
	} else {
		bc.notifyStateChange()
	}
}

func (bc *CaptchaProtect) getClientIP(req *http.Request) (string, string) {
	ip := req.Header.Get(bc.config.IPForwardedHeader)
	if bc.config.IPForwardedHeader != "" && ip != "" {
		components := strings.Split(ip, ",")
		depth := bc.config.IPDepth
		ip = ""
		for i := len(components) - 1; i >= 0; i-- {
			_ip := strings.TrimSpace(components[i])
			if helper.IsIpExcluded(_ip, bc.exemptIps) {
				continue
			}
			if depth == 0 {
				ip = _ip
				break
			}
			depth--
		}
		if ip == "" {
			log.Debug("No non-exempt IPs in header. req.RemoteAddr", "ipDepth", bc.config.IPDepth, bc.config.IPForwardedHeader, req.Header.Get(bc.config.IPForwardedHeader))
			ip = req.RemoteAddr
		}
	} else {
		if bc.config.IPForwardedHeader != "" {
			log.Debug("Received a blank header value. Defaulting to real IP")
		}
		ip = req.RemoteAddr
	}
	if strings.Contains(ip, ":") {
		host, _, _ := net.SplitHostPort(ip)
		ip = host
	}

	return bc.ParseIp(ip)
}

func (bc *CaptchaProtect) ParseIp(ip string) (string, string) {
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return ip, ip
	}

	// For IPv4 addresses
	if parsedIP.To4() != nil {
		subnet := parsedIP.Mask(bc.ipv4Mask)
		return ip, subnet.String()
	}

	// For IPv6 addresses
	if parsedIP.To16() != nil {
		subnet := parsedIP.Mask(bc.ipv6Mask)
		return ip, subnet.String()
	}

	log.Warn("Unknown ip version", "ip", ip)

	return ip, ip
}

func (bc *CaptchaProtect) SetIpv4Mask(m int) error {
	if m < 8 || m > 32 {
		return fmt.Errorf("invalid ipv4 mask: %d. Must be between 8 and 32", m)
	}
	bc.ipv4Mask = net.CIDRMask(m, 32)

	return nil
}

func (bc *CaptchaProtect) SetIpv6Mask(m int) error {
	if m < 8 || m > 128 {
		return fmt.Errorf("invalid ipv6 mask: %d. Must be between 8 and 128", m)
	}
	bc.ipv6Mask = net.CIDRMask(m, 128)

	return nil
}

func (bc *CaptchaProtect) isGoodBot(req *http.Request, clientIP string) bool {
	if bc.config.ProtectParameters == "true" {
		if len(req.URL.Query()) > 0 {
			return false
		}
	}

	bot, ok := bc.botCache.Get(clientIP)
	if ok {
		return bot.(bool)
	}

	v := helper.IsIpGoodBot(clientIP, bc.config.GoodBots)
	bc.botCache.Set(clientIP, v, lru.DefaultExpiration)
	bc.notifyStateChange()
	return v
}

func (bc *CaptchaProtect) SetExemptIps(exemptIps []*net.IPNet) {
	bc.exemptIps = exemptIps
}

// log a warning if protected methods contains an invalid method
func (c *Config) ParseHttpMethods() {
	for _, method := range c.ProtectHttpMethods {
		switch method {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE":
			continue
		default:
			log.Warn("unknown http method", "method", method)
		}
	}
}

func (bc *CaptchaProtect) saveStateOnChange(ctx context.Context) {
	// Test file access on startup
	file, err := os.OpenFile(bc.config.PersistentStateFile, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Error("Unable to save state. Could not open or create file", "stateFile", bc.config.PersistentStateFile, "err", err)
		return
	}
	file.Close()

	for {
		select {
		case <-bc.stateChanged:
			log.Debug("State changed, saving state")
			bc.saveStateWithLock()
		case <-ctx.Done():
			log.Debug("Context cancelled, stopping saveStateOnChange")
			// Save final state before exiting
			bc.saveStateWithLock()
			return
		}
	}
}

func (bc *CaptchaProtect) saveStateWithLock() {
	bc.stateMutex.Lock()
	defer bc.stateMutex.Unlock()

	// Read current file state and reconcile differences
	currentState := bc.readStateFromFile()
	newState := state.GetState(bc.rateCache.Items(), bc.botCache.Items(), bc.verifiedCache.Items())

	// Reconcile the states - merge current file state with new state
	reconciledState := bc.reconcileStates(currentState, newState)

	// Acquire file lock and write
	err := bc.writeStateToFile(reconciledState)
	if err != nil {
		log.Error("failed saving state data", "err", err)
	}
}

func (bc *CaptchaProtect) writeStateToFile(state state.State) error {
	// Create file lock
	lock := filelock.New(bc.config.PersistentStateFile)

	// Acquire exclusive lock
	err := lock.Lock()
	if err != nil {
		return fmt.Errorf("unable to acquire file lock: %w", err)
	}
	defer lock.Unlock()

	file, err := os.OpenFile(bc.config.PersistentStateFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("unable to open state file: %w", err)
	}
	defer file.Close()

	jsonData, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed marshalling state data: %w", err)
	}

	_, err = file.Write(jsonData)
	if err != nil {
		return fmt.Errorf("failed writing state data: %w", err)
	}

	return nil
}

func (bc *CaptchaProtect) readStateFromFile() state.State {
	// Create file lock
	lock := filelock.New(bc.config.PersistentStateFile)

	// Acquire shared lock for reading
	err := lock.Lock()
	if err != nil {
		log.Error("Unable to acquire file lock for reading", "err", err)
		return state.State{}
	}
	defer lock.Unlock()

	file, err := os.OpenFile(bc.config.PersistentStateFile, os.O_RDONLY, 0644)
	if err != nil {
		log.Debug("Unable to open state file for reading", "err", err)
		return state.State{}
	}
	defer file.Close()

	fileContent, err := os.ReadFile(bc.config.PersistentStateFile)
	if err != nil || len(fileContent) == 0 {
		log.Debug("Failed to read state file content", "err", err)
		return state.State{}
	}

	var fileState state.State
	err = json.Unmarshal(fileContent, &fileState)
	if err != nil {
		log.Error("Failed to unmarshal state file", "err", err)
		return state.State{}
	}

	return fileState
}

func (bc *CaptchaProtect) reconcileStates(fileState, memoryState state.State) state.State {
	// Start with memory state as base
	reconciledState := memoryState

	// Initialize maps if they're nil
	if reconciledState.Rate == nil {
		reconciledState.Rate = make(map[string]uint)
	}
	if reconciledState.Bots == nil {
		reconciledState.Bots = make(map[string]bool)
	}
	if reconciledState.Verified == nil {
		reconciledState.Verified = make(map[string]bool)
	}
	if reconciledState.Memory == nil {
		reconciledState.Memory = make(map[string]uintptr)
	}

	// Merge file state into memory state
	// For rate limits, take the higher value (more restrictive)
	for ip, fileRate := range fileState.Rate {
		if memoryRate, exists := reconciledState.Rate[ip]; exists {
			if fileRate > memoryRate {
				reconciledState.Rate[ip] = fileRate
			}
		} else {
			reconciledState.Rate[ip] = fileRate
		}
	}

	// For bots, merge both states (union)
	for ip, isBot := range fileState.Bots {
		if _, exists := reconciledState.Bots[ip]; !exists {
			reconciledState.Bots[ip] = isBot
		}
	}

	// For verified, merge both states (union)
	for ip, isVerified := range fileState.Verified {
		if _, exists := reconciledState.Verified[ip]; !exists {
			reconciledState.Verified[ip] = isVerified
		}
	}

	return reconciledState
}

func (bc *CaptchaProtect) notifyStateChange() {
	if bc.stateChanged != nil {
		select {
		case bc.stateChanged <- struct{}{}:
		default:
			// Channel is full, state change already pending
		}
	}
}

func (bc *CaptchaProtect) loadState() {
	bc.stateMutex.Lock()
	defer bc.stateMutex.Unlock()

	// Create file lock
	lock := filelock.New(bc.config.PersistentStateFile)

	// Acquire lock for reading
	err := lock.Lock()
	if err != nil {
		log.Error("Unable to acquire file lock during load", "err", err)
		return
	}
	defer lock.Unlock()

	file, err := os.OpenFile(bc.config.PersistentStateFile, os.O_RDONLY, 0644)
	if err != nil {
		log.Warn("Failed to open state file for loading", "err", err)
		return
	}
	defer file.Close()

	fileContent, err := os.ReadFile(bc.config.PersistentStateFile)
	if err != nil || len(fileContent) == 0 {
		log.Warn("Failed to load state file content", "err", err)
		return
	}

	var state state.State
	err = json.Unmarshal(fileContent, &state)
	if err != nil {
		log.Error("Failed to unmarshal state file", "err", err)
		return
	}

	for k, v := range state.Rate {
		bc.rateCache.Set(k, v, lru.DefaultExpiration)
	}

	for k, v := range state.Bots {
		bc.botCache.Set(k, v, lru.DefaultExpiration)
	}

	for k, v := range state.Verified {
		bc.verifiedCache.Set(k, v, lru.DefaultExpiration)
	}

	log.Info("Loaded previous state",
		"rateEntries", len(state.Rate),
		"botEntries", len(state.Bots),
		"verifiedEntries", len(state.Verified),
		"stateFile", bc.config.PersistentStateFile)
}

func (bc *CaptchaProtect) reloadStateIfNeeded() {
	// Check if 5 seconds have passed since last reload (with read lock for thread safety)
	bc.stateMutex.RLock()
	lastReload := bc.lastStateReload
	bc.stateMutex.RUnlock()

	now := time.Now()
	if now.Sub(lastReload) < 5*time.Second {
		return
	}

	// Double-check with write lock to avoid race conditions
	bc.stateMutex.Lock()
	if now.Sub(bc.lastStateReload) < 5*time.Second {
		bc.stateMutex.Unlock()
		return
	}
	bc.lastStateReload = now
	bc.stateMutex.Unlock()

	bc.reloadStateFromFile()
}

func (bc *CaptchaProtect) reloadStateFromFile() {
	bc.stateMutex.Lock()
	defer bc.stateMutex.Unlock()

	// Read current file state
	fileState := bc.readStateFromFile()
	if len(fileState.Rate) == 0 && len(fileState.Bots) == 0 && len(fileState.Verified) == 0 {
		// No state to reload
		return
	}

	// Get current memory state
	memoryState := state.GetState(bc.rateCache.Items(), bc.botCache.Items(), bc.verifiedCache.Items())

	// Reconcile file state with memory state
	reconciledState := bc.reconcileStates(fileState, memoryState)

	// Clear current caches
	bc.rateCache.Flush()
	bc.botCache.Flush()
	bc.verifiedCache.Flush()

	// Load reconciled state into caches
	for k, v := range reconciledState.Rate {
		bc.rateCache.Set(k, v, lru.DefaultExpiration)
	}

	for k, v := range reconciledState.Bots {
		bc.botCache.Set(k, v, lru.DefaultExpiration)
	}

	for k, v := range reconciledState.Verified {
		bc.verifiedCache.Set(k, v, lru.DefaultExpiration)
	}

	log.Debug("Reloaded state from file",
		"rateEntries", len(reconciledState.Rate),
		"botEntries", len(reconciledState.Bots),
		"verifiedEntries", len(reconciledState.Verified))
}

func (bc *CaptchaProtect) ChallengeOnPage() bool {
	return bc.config.ChallengeURL == "?challenge=true"
}
