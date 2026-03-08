package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	cookieName = "openclaw_auth"
	cookieMaxAge = 30 * 24 * 60 * 60 // 30 days
)

const controlUICustomizations = `<style id="openclaw-render-tool-card-override">
html.oc-hide-tool-cards .chat-group.tool {
	display: none !important;
}
html.oc-hide-tool-cards .chat-tool-card {
	display: none !important;
}
html.oc-hide-tool-cards .chat-group.assistant:has(.chat-tool-card):not(:has(.chat-text)):not(:has(.chat-message-images)):not(:has(.chat-thinking)) {
	display: none !important;
}
</style>
<script id="openclaw-render-tool-card-script">
(() => {
	const settingsKey = "openclaw.control.settings.v1";
	const className = "oc-hide-tool-cards";

	function syncToolCardVisibility() {
		try {
			const raw = window.localStorage.getItem(settingsKey);
			const parsed = raw ? JSON.parse(raw) : null;
			const hide = parsed && parsed.chatShowThinking === false;
			document.documentElement.classList.toggle(className, Boolean(hide));
		} catch {
			document.documentElement.classList.remove(className);
		}
	}

	syncToolCardVisibility();
	window.addEventListener("storage", syncToolCardVisibility);
	window.addEventListener("focus", syncToolCardVisibility);
	document.addEventListener(
		"click",
		() => window.setTimeout(syncToolCardVisibility, 0),
		true,
	);
})();
</script>`

var (
	port         = envOr("PORT", "10000")
	stateDir     = envOr("OPENCLAW_STATE_DIR", "/data/.openclaw")
	workspaceDir = envOr("OPENCLAW_WORKSPACE_DIR", "/data/workspace")
	gatewayToken = os.Getenv("OPENCLAW_GATEWAY_TOKEN")
	gatewayPort  = "18789"

	gatewayReady atomic.Bool
	gatewayCmd   *exec.Cmd
	cmdMu        sync.Mutex

	// cookieSecret is used to sign auth cookies (generated on startup)
	cookieSecret []byte

	// Rate limiting for auth attempts
	authAttempts   = make(map[string][]time.Time)
	authAttemptsMu sync.Mutex
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)

	if gatewayToken == "" {
		log.Printf("Warning: OPENCLAW_GATEWAY_TOKEN not set - access will be blocked until configured")
	}

	// Derive cookie signing secret from gateway token (survives restarts)
	// Falls back to random if no token configured (auth blocked anyway)
	if gatewayToken != "" {
		hash := sha256.Sum256([]byte("openclaw-cookie:" + gatewayToken))
		cookieSecret = hash[:]
	} else {
		cookieSecret = make([]byte, 32)
		rand.Read(cookieSecret)
	}

	ensureDirs()
	ensureConfigured()
	go startGateway()
	go pollGatewayHealth()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/auth", handleAuth)
	mux.HandleFunc("/", handleProxy)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	go func() {
		log.Printf("Proxy listening on :%s", port)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Graceful shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig

	log.Println("Shutting down...")
	cmdMu.Lock()
	if gatewayCmd != nil && gatewayCmd.Process != nil {
		gatewayCmd.Process.Signal(syscall.SIGTERM)
	}
	cmdMu.Unlock()
	server.Close()
}

func ensureDirs() {
	for _, dir := range []string{stateDir, workspaceDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("Warning: could not create %s: %v", dir, err)
		}
	}
}

func ensureConfigured() {
	configPath := stateDir + "/openclaw.json"
	if _, err := os.Stat(configPath); err == nil {
		log.Printf("Config exists at %s, skipping onboard", configPath)
		applyRequiredConfig()
		applyCpaConfigBootstrap(configPath)
		return
	}

	// Run onboard to properly initialize workspace + config
	log.Printf("Running openclaw onboard to initialize...")
	args := []string{
		"onboard",
		"--non-interactive",
		"--accept-risk",
		"--flow", "manual",
		"--skip-health",
		"--no-install-daemon",
		"--skip-channels",
		"--skip-skills",
		"--workspace", workspaceDir,
		"--gateway-bind", "loopback",
		"--gateway-port", gatewayPort,
		"--gateway-auth", "token",
	}
	if gatewayToken != "" {
		args = append(args, "--gateway-token", gatewayToken)
	}

	// Pass API keys if present in environment (priority order for primary auth profile)
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		args = append(args, "--auth-choice", "apiKey", "--anthropic-api-key", key)
	} else if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		args = append(args, "--auth-choice", "openai-api-key", "--openai-api-key", key)
	} else if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		args = append(args, "--auth-choice", "gemini-api-key", "--gemini-api-key", key)
	} else if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		args = append(args, "--auth-choice", "openrouter-api-key", "--openrouter-api-key", key)
	} else if key := os.Getenv("MOONSHOT_API_KEY"); key != "" {
		args = append(args, "--auth-choice", "moonshot-api-key", "--moonshot-api-key", key)
	} else if key := os.Getenv("MINIMAX_API_KEY"); key != "" {
		args = append(args, "--auth-choice", "minimax-api", "--minimax-api-key", key)
	} else {
		// No API key - skip auth setup, user can configure via Control UI
		args = append(args, "--auth-choice", "skip")
	}

	cmd := exec.Command("/usr/local/bin/openclaw", args...)
	cmd.Env = append(os.Environ(),
		"OPENCLAW_STATE_DIR="+stateDir,
		"OPENCLAW_WORKSPACE_DIR="+workspaceDir,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Printf("Warning: onboard failed (%v), creating minimal config as fallback", err)
		createMinimalConfig(configPath)
		applyRequiredConfig()
		applyCpaConfigBootstrap(configPath)
		return
	}

	log.Printf("Onboard completed, applying additional config...")
	applyRequiredConfig()
	applyCpaConfigBootstrap(configPath)
}

func createMinimalConfig(configPath string) {
	config := []byte(`{
  "gateway": {
    "mode": "local",
    "controlUi": {
      "allowInsecureAuth": true
    }
  }
}
`)
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		log.Printf("Warning: could not create minimal config: %v", err)
	} else {
		log.Printf("Created minimal config at %s", configPath)
	}
}

func applyRequiredConfig() {
	// Ensure controlUi.allowInsecureAuth is set for remote browser access
	configs := [][]string{
		{"config", "set", "gateway.controlUi.allowInsecureAuth", "true"},
		{"config", "set", "tools.profile", "full"},
	}

	// Allow WebSocket connections from Render's external hostname
	// RENDER_EXTERNAL_HOSTNAME is provided by Render (e.g., "service-id.onrender.com")
	if renderHost := os.Getenv("RENDER_EXTERNAL_HOSTNAME"); renderHost != "" {
		origin := fmt.Sprintf(`["https://%s"]`, renderHost)
		configs = append(configs, []string{"config", "set", "gateway.controlUi.allowedOrigins", origin})
		log.Printf("Allowing WebSocket origin: https://%s", renderHost)
	}

	for _, args := range configs {
		cmd := exec.Command("/usr/local/bin/openclaw", args...)
		cmd.Env = append(os.Environ(),
			"OPENCLAW_STATE_DIR="+stateDir,
			"OPENCLAW_WORKSPACE_DIR="+workspaceDir,
		)
		if err := cmd.Run(); err != nil {
			log.Printf("Warning: config set failed for %v: %v", args, err)
		}
	}
}

type cpaModelEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cpaBootstrapSettings struct {
	envConfigured bool
	createProvider bool
	baseURL       string
	apiKey        string
	api           string
	models        []cpaModelEntry
	primaryModel  string
	coderModel    string
}

var defaultCpaModels = []cpaModelEntry{
	{ID: "gpt-5.4", Name: "ChatGPT 5.4"},
	{ID: "gpt-5", Name: "ChatGPT 5"},
	{ID: "gpt-5-codex", Name: "GPT-5 Codex"},
	{ID: "gemini-3-flash", Name: "Gemini 3 Flash"},
}

var defaultCpaAliases = map[string]string{
	"cpa/gpt-5.4":        "chatgpt-5.4",
	"cpa/gpt-5":          "chatgpt-5",
	"cpa/gpt-5-codex":    "codex",
	"cpa/gemini-3-flash": "gemini-flash",
}

func applyCpaConfigBootstrap(configPath string) {
	settings := resolveCpaBootstrapSettings()

	raw, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("Skipping CPA bootstrap, could not read %s: %v", configPath, err)
		return
	}
	if len(raw) >= 3 && raw[0] == 0xef && raw[1] == 0xbb && raw[2] == 0xbf {
		raw = raw[3:]
	}

	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Printf("Skipping CPA bootstrap, invalid JSON in %s: %v", configPath, err)
		return
	}

	models, ok := ensureObject(cfg, "models")
	if !ok {
		log.Printf("Skipping CPA bootstrap, models section is invalid")
		return
	}
	providers, ok := ensureObject(models, "providers")
	if !ok {
		log.Printf("Skipping CPA bootstrap, providers section is invalid")
		return
	}

	changed := false
	var cpa map[string]any
	if cpaRaw, exists := providers["cpa"]; exists {
		cpa, ok = cpaRaw.(map[string]any)
		if !ok {
			log.Printf("Skipping CPA bootstrap, models.providers.cpa is invalid")
			return
		}
	} else if settings.createProvider {
		cpa = map[string]any{}
		providers["cpa"] = cpa
		changed = true
	} else {
		log.Printf("Skipping CPA bootstrap, models.providers.cpa is not configured")
		return
	}

	if settings.envConfigured {
		if currentMode, _ := models["mode"].(string); strings.TrimSpace(currentMode) == "" {
			models["mode"] = "replace"
			changed = true
		}
		if applyCpaProviderSettings(cpa, settings) {
			changed = true
		}
	}

	if nextModels, modelChanged := mergeCpaModels(cpa["models"], settings.models, !settings.envConfigured); modelChanged {
		cpa["models"] = nextModels
		changed = true
	}

	agents, ok := ensureObject(cfg, "agents")
	if !ok {
		log.Printf("Skipping CPA bootstrap, agents section is invalid")
		return
	}
	defaults, ok := ensureObject(agents, "defaults")
	if !ok {
		log.Printf("Skipping CPA bootstrap, agents.defaults section is invalid")
		return
	}
	modelCfg, ok := ensureObject(defaults, "model")
	if !ok {
		log.Printf("Skipping CPA bootstrap, agents.defaults.model section is invalid")
		return
	}
	desiredPrimary := "cpa/" + settings.primaryModel
	if primary, _ := modelCfg["primary"].(string); settings.envConfigured || strings.TrimSpace(primary) == "" || strings.HasPrefix(primary, "cpa/") {
		if primary != desiredPrimary {
			modelCfg["primary"] = desiredPrimary
			changed = true
		}
	}

	if ensureModelAliases(defaults, buildCpaAliases(settings.models)) {
		changed = true
	}
	if ensureCoderModel(agents, "cpa/"+settings.coderModel) {
		changed = true
	}

	if !changed {
		log.Printf("CPA bootstrap: config already up to date")
		return
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("Skipping CPA bootstrap, could not encode config: %v", err)
		return
	}
	out = append(out, '\n')
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		log.Printf("Skipping CPA bootstrap, could not write %s: %v", configPath, err)
		return
	}
	log.Printf("Applied CPA bootstrap config to %s", configPath)
}

func resolveCpaBootstrapSettings() cpaBootstrapSettings {
	rawModels := strings.TrimSpace(os.Getenv("CPA_MODELS"))
	defaultModel := strings.TrimSpace(os.Getenv("CPA_DEFAULT_MODEL"))
	coderModel := strings.TrimSpace(os.Getenv("CPA_CODER_MODEL"))

	settings := cpaBootstrapSettings{
		baseURL:      strings.TrimSpace(os.Getenv("CPA_BASE_URL")),
		apiKey:       strings.TrimSpace(os.Getenv("CPA_API_KEY")),
		api:          strings.TrimSpace(os.Getenv("CPA_API")),
		models:       copyCpaModels(defaultCpaModels),
		primaryModel: "gpt-5.4",
		coderModel:   "gpt-5-codex",
	}

	if parsedModels, ok := parseCpaModels(rawModels); ok {
		settings.models = parsedModels
	}
	if defaultModel != "" {
		settings.primaryModel = defaultModel
	}
	settings.models = appendCpaModel(settings.models, settings.primaryModel)

	switch {
	case coderModel != "":
		settings.coderModel = coderModel
	case hasCpaModel(settings.models, "gpt-5-codex"):
		settings.coderModel = "gpt-5-codex"
	default:
		settings.coderModel = settings.primaryModel
	}
	settings.models = appendCpaModel(settings.models, settings.coderModel)

	if settings.api == "" && (settings.baseURL != "" || settings.apiKey != "" || rawModels != "") {
		settings.api = "openai-responses"
	}
	settings.createProvider = settings.baseURL != "" || settings.apiKey != "" || rawModels != ""
	settings.envConfigured = settings.createProvider || settings.api != "" || defaultModel != "" || coderModel != ""

	return settings
}

func ensureObject(parent map[string]any, key string) (map[string]any, bool) {
	if current, exists := parent[key]; exists {
		asMap, ok := current.(map[string]any)
		return asMap, ok
	}
	next := map[string]any{}
	parent[key] = next
	return next, true
}

func copyCpaModels(source []cpaModelEntry) []cpaModelEntry {
	cloned := make([]cpaModelEntry, len(source))
	copy(cloned, source)
	return cloned
}

func parseCpaModels(raw string) ([]cpaModelEntry, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}

	if strings.HasPrefix(raw, "[") {
		var parsed []cpaModelEntry
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			log.Printf("Ignoring CPA_MODELS, invalid JSON: %v", err)
			return nil, false
		}
		parsed = sanitizeCpaModels(parsed)
		if len(parsed) == 0 {
			log.Printf("Ignoring CPA_MODELS, no valid model IDs found")
			return nil, false
		}
		return parsed, true
	}

	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	})
	parsed := make([]cpaModelEntry, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}

		id := field
		name := ""
		switch {
		case strings.Contains(field, "="):
			parts := strings.SplitN(field, "=", 2)
			id = strings.TrimSpace(parts[0])
			name = strings.TrimSpace(parts[1])
		case strings.Contains(field, "|"):
			parts := strings.SplitN(field, "|", 2)
			id = strings.TrimSpace(parts[0])
			name = strings.TrimSpace(parts[1])
		}
		if id == "" {
			continue
		}
		if name == "" {
			name = cpaDisplayName(id)
		}
		parsed = append(parsed, cpaModelEntry{ID: id, Name: name})
	}

	parsed = sanitizeCpaModels(parsed)
	if len(parsed) == 0 {
		log.Printf("Ignoring CPA_MODELS, no valid model IDs found")
		return nil, false
	}
	return parsed, true
}

func sanitizeCpaModels(source []cpaModelEntry) []cpaModelEntry {
	sanitized := make([]cpaModelEntry, 0, len(source))
	seen := map[string]struct{}{}
	for _, model := range source {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = cpaDisplayName(id)
		}
		sanitized = append(sanitized, cpaModelEntry{ID: id, Name: name})
		seen[id] = struct{}{}
	}
	return sanitized
}

func cpaDisplayName(id string) string {
	for _, model := range defaultCpaModels {
		if model.ID == id {
			return model.Name
		}
	}
	return id
}

func appendCpaModel(models []cpaModelEntry, id string) []cpaModelEntry {
	id = strings.TrimSpace(id)
	if id == "" || hasCpaModel(models, id) {
		return models
	}
	return append(models, cpaModelEntry{ID: id, Name: cpaDisplayName(id)})
}

func applyCpaProviderSettings(cpa map[string]any, settings cpaBootstrapSettings) bool {
	changed := false
	if settings.baseURL != "" {
		if current, _ := cpa["baseUrl"].(string); current != settings.baseURL {
			cpa["baseUrl"] = settings.baseURL
			changed = true
		}
	}
	if settings.apiKey != "" {
		if current, _ := cpa["apiKey"].(string); current != settings.apiKey {
			cpa["apiKey"] = settings.apiKey
			changed = true
		}
	}
	if settings.api != "" {
		if current, _ := cpa["api"].(string); current != settings.api {
			cpa["api"] = settings.api
			changed = true
		}
	}
	return changed
}

func mergeCpaModels(raw any, desired []cpaModelEntry, keepExtras bool) ([]map[string]any, bool) {
	extras := []map[string]any{}
	existingNames := map[string]string{}
	existingIDs := map[string]struct{}{}
	changed := raw == nil

	if items, ok := raw.([]any); ok {
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				changed = true
				continue
			}
			id, _ := entry["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" {
				changed = true
				continue
			}
			name, _ := entry["name"].(string)
			existingIDs[id] = struct{}{}
			existingNames[id] = strings.TrimSpace(name)
			if hasCpaModel(desired, id) {
				continue
			}
			if keepExtras {
				extras = append(extras, entry)
			} else {
				changed = true
			}
		}
	} else if raw != nil {
		changed = true
	}

	next := make([]map[string]any, 0, len(desired)+len(extras))
	for _, model := range desired {
		next = append(next, map[string]any{
			"id":   model.ID,
			"name": model.Name,
		})
		if _, exists := existingIDs[model.ID]; !exists {
			changed = true
			continue
		}
		if currentName := existingNames[model.ID]; currentName != model.Name {
			changed = true
		}
	}
	next = append(next, extras...)
	return next, changed
}

func hasCpaModel(models []cpaModelEntry, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}

func buildCpaAliases(models []cpaModelEntry) map[string]string {
	aliases := make(map[string]string, len(models))
	for _, model := range models {
		fullID := "cpa/" + model.ID
		if alias, exists := defaultCpaAliases[fullID]; exists {
			aliases[fullID] = alias
			continue
		}
		aliases[fullID] = normalizeCpaAlias(model.ID)
	}
	return aliases
}

func normalizeCpaAlias(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("/", "-", "\\", "-", "_", "-", " ", "-", "|", "-", "=", "-", ":", "-", ".", "-")
	value = replacer.Replace(value)
	value = strings.Trim(value, "-")
	if value == "" {
		return "cpa-model"
	}
	return value
}

func ensureModelAliases(defaults map[string]any, aliases map[string]string) bool {
	modelsRaw := defaults["models"]
	var models map[string]any
	switch typed := modelsRaw.(type) {
	case map[string]any:
		models = typed
	case nil:
		models = map[string]any{}
		defaults["models"] = models
	default:
		models = map[string]any{}
		defaults["models"] = models
	}

	changed := false
	for key, alias := range aliases {
		currentRaw, exists := models[key]
		if !exists {
			models[key] = map[string]any{"alias": alias}
			changed = true
			continue
		}
		current, ok := currentRaw.(map[string]any)
		if !ok {
			models[key] = map[string]any{"alias": alias}
			changed = true
			continue
		}
		if existingAlias, _ := current["alias"].(string); strings.TrimSpace(existingAlias) == "" {
			current["alias"] = alias
			changed = true
		}
	}
	return changed
}

func ensureCoderModel(agents map[string]any, desiredModel string) bool {
	listRaw, ok := agents["list"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, item := range listRaw {
		agent, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := agent["id"].(string)
		if id != "coder" {
			continue
		}
		if current, _ := agent["model"].(string); current != desiredModel {
			agent["model"] = desiredModel
			changed = true
		}
	}
	return changed
}

func startGateway() {
	cmdMu.Lock()
	defer cmdMu.Unlock()

	if gatewayCmd != nil {
		return
	}

	log.Printf("Starting openclaw gateway on port %s...", gatewayPort)

	cmd := exec.Command("/usr/local/bin/openclaw", "gateway", "run",
		"--port", gatewayPort,
		"--bind", "loopback",
	)
	cmd.Env = append(os.Environ(),
		"OPENCLAW_STATE_DIR="+stateDir,
		"OPENCLAW_WORKSPACE_DIR="+workspaceDir,
		"OPENCLAW_GATEWAY_PORT="+gatewayPort,
	)
	if gatewayToken != "" {
		cmd.Env = append(cmd.Env, "OPENCLAW_GATEWAY_TOKEN="+gatewayToken)
	}

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start gateway: %v", err)
		return
	}
	gatewayCmd = cmd

	// Stream output
	go streamLog("gateway", stdout)
	go streamLog("gateway:err", stderr)

	go func() {
		err := cmd.Wait()
		log.Printf("Gateway exited: %v", err)
		cmdMu.Lock()
		gatewayCmd = nil
		gatewayReady.Store(false)
		cmdMu.Unlock()
		// Restart after delay
		time.Sleep(3 * time.Second)
		go startGateway()
	}()
}

func streamLog(prefix string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		log.Printf("[%s] %s", prefix, scanner.Text())
	}
}

func pollGatewayHealth() {
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		time.Sleep(1 * time.Second)
		resp, err := client.Get("http://127.0.0.1:" + gatewayPort + "/openclaw")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				if !gatewayReady.Load() {
					log.Println("Gateway is ready")
					gatewayReady.Store(true)
				}
			}
		}
	}
}

// --- Handlers ---

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ready := gatewayReady.Load()
	fmt.Fprintf(w, `{"ok":%t,"gateway":%t}`, ready, ready)
}

// --- Rate limiting ---

const (
	rateLimitWindow   = time.Minute
	rateLimitMaxTries = 5
)

func isRateLimited(ip string) bool {
	authAttemptsMu.Lock()
	defer authAttemptsMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rateLimitWindow)

	// Filter to recent attempts only
	recent := authAttempts[ip][:0]
	for _, t := range authAttempts[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	authAttempts[ip] = recent

	return len(recent) >= rateLimitMaxTries
}

func recordAuthAttempt(ip string) {
	authAttemptsMu.Lock()
	defer authAttemptsMu.Unlock()
	authAttempts[ip] = append(authAttempts[ip], time.Now())
}

// --- Auth cookie helpers ---

func computeAuthCookie(token string) string {
	mac := hmac.New(sha256.New, cookieSecret)
	mac.Write([]byte(token))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func isValidAuthCookie(r *http.Request) bool {
	if gatewayToken == "" {
		// No token configured - deny access
		return false
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	expected := computeAuthCookie(gatewayToken)
	return hmac.Equal([]byte(cookie.Value), []byte(expected))
}

func setAuthCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    computeAuthCookie(token),
		Path:     "/",
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// --- Landing page ---

const landingPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>OpenClaw - Authentication</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: 'Helvetica Neue', sans-serif;
      background: #12141a;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 20px;
    }
    .card {
      background: #fff;
      box-shadow: 0 4px 24px rgba(0,0,0,0.2);
      padding: 40px;
      max-width: 400px;
      width: 100%;
    }
    h1 {
      font-size: 30px;
      margin-bottom: 12px;
      color: #1a1a2e;
      font-weight: 400;
    }
    .subtitle {
      margin-bottom: 24px;
      font-size: 14px;
    }
    label {
      display: block;
      font-size: 14px;
      font-weight: 500;
      margin-bottom: 8px;
      color: #333;
    }
    input[type="password"] {
      width: 100%;
      padding: 12px 16px;
      border: 1px solid #ddd;
      font-size: 16px;
      margin-bottom: 16px;
    }
    input[type="password"]:focus {
      outline: none;
      border-color: #ff5c5c;
    }
    button {
      width: 100%;
      padding: 12px 24px;
      background: #ff5c5c;
      color: #fff;
      border: none;
      font-size: 16px;
      font-weight: 500;
      cursor: pointer;
    }
    button:hover { background: #ff7070; }
    a, code {
      color: #ff5c5c;
      font-size: 13px;
    }
    .error {
      background: #fee;
      color: #c00;
      padding: 12px;
      margin-bottom: 16px;
      font-size: 14px;
    }
    .hint {
      margin-top: 16px;
      font-size: 12px;
      color: #888;
      text-align: center;
    }
  </style>
</head>
<body>
  <div class="card">
    <h1>OpenClaw on Render</h1>
    <p class="subtitle">Provide your <code>OPENCLAW_GATEWAY_TOKEN</code> to access the Control UI.</p>
    {{ERROR}}
    <form method="POST" action="/auth">
      <label for="token">Gateway Token</label>
      <input type="password" id="token" name="token" placeholder="Enter token..." required autofocus>
      <button type="submit">Continue</button>
    </form>
    <p class="hint">Copy your token from your service's <strong>Environment</strong> panel in the <a href="https://dashboard.render.com" target="_blank">Render Dashboard</a>.</p>
  </div>
</body>
</html>`

func handleLandingPage(w http.ResponseWriter, r *http.Request, errorMsg string) {
	// If no token is configured, show configuration error instead of login form
	if gatewayToken == "" {
		handleConfigError(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := landingPageHTML
	if errorMsg != "" {
		html = strings.Replace(html, "{{ERROR}}", `<div class="error">`+errorMsg+`</div>`, 1)
	} else {
		html = strings.Replace(html, "{{ERROR}}", "", 1)
	}
	w.Write([]byte(html))
}

const configErrorHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>OpenClaw - Configuration Required</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: 'Helvetica Neue', sans-serif;
      background: #12141a;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 20px;
    }
    .card {
      background: #fff;
      box-shadow: 0 4px 24px rgba(0,0,0,0.2);
      padding: 40px;
      max-width: 480px;
      width: 100%;
    }
    h1 {
      font-size: 30px;
      margin-bottom: 12px;
      color: #1a1a2e;
      font-weight: 400;
    }
    p {
      margin-bottom: 16px;
      font-size: 14px;
      line-height: 1.5;
    }
    h2 {
      font-weight: bold;
      margin-top: 24px;
      margin-bottom: 24px;
      font-size: 14px;
    }
    code {
      font-size: 13px;
      color: #ff5c5c;
    }
    ol {
      margin: 20px 0;
      padding-left: 20px;
      font-size: 14px;
    }
    li {
      line-height: 1.3;
      padding-bottom: 10px;
    }
    a {
      color: #ff5c5c;
    }
  </style>
</head>
<body>
  <div class="card">
    <h1>OpenClaw on Render</h1>
    <h2>Missing Configuration</h2>
    <p>This OpenClaw instance does not set an <code>OPENCLAW_GATEWAY_TOKEN</code> environment variable. This token is required to access the Control UI.</p>
    <ol>
      <li>Open the <a href="https://dashboard.render.com" target="_blank">Render Dashboard</a>.</li>
      <li>Navigate to your service's <strong>Environment</strong> page.</li>
      <li>Create a new environment variable with the key <code>OPENCLAW_GATEWAY_TOKEN</code> and a value of your choice.</li>
      <li>Click <strong>Save and Deploy</strong>.</li>
    </ol>
    <p>After the deployment completes, refresh this page to provide your token and log in.</p>
  </div>
</body>
</html>`

func handleConfigError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(configErrorHTML))
}

func handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Block if no token is configured
	if gatewayToken == "" {
		handleConfigError(w)
		return
	}

	// Rate limit by IP
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		ip = r.RemoteAddr
	}
	if isRateLimited(ip) {
		handleLandingPage(w, r, "Too many attempts. Please wait a minute.")
		return
	}

	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		handleLandingPage(w, r, "Please enter a token")
		return
	}

	// Validate token (constant-time comparison to prevent timing attacks)
	if !hmac.Equal([]byte(token), []byte(gatewayToken)) {
		recordAuthAttempt(ip)
		handleLandingPage(w, r, "Invalid token")
		return
	}

	// Set auth cookie and redirect to Control UI with token
	setAuthCookie(w, token)
	redirectURL := "/openclaw?token=" + url.QueryEscape(token)
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

// Strip proxy headers so the gateway sees requests as local.
// This prevents "untrusted proxy" warnings since the gateway runs on loopback.
var proxyHeaders = []string{
	"Forwarded",
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Port",
	"X-Forwarded-Proto",
	"X-Forwarded-Server",
	"X-Forwarded-Ssl",
	"X-Real-Ip",
	"X-Client-Ip",
	"Cf-Connecting-Ip",
	"True-Client-Ip",
}

func stripProxyHeaders(r *http.Request) {
	for _, h := range proxyHeaders {
		r.Header.Del(h)
	}
	// Override Host header so gateway sees request as fully local
	// (prevents "non-local Host header" warnings)
	r.Host = "127.0.0.1:" + gatewayPort
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	// Check auth cookie (skip for health endpoint, already handled separately)
	if !isValidAuthCookie(r) {
		// Show landing page for root, redirect others to root
		if r.URL.Path == "/" || r.URL.Path == "" {
			handleLandingPage(w, r, "")
		} else {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
		return
	}

	if !gatewayReady.Load() {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"gateway not ready","retry":true}`))
		return
	}

	// Strip proxy headers so gateway sees requests as local
	stripProxyHeaders(r)

	// WebSocket upgrade
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		proxyWebSocket(w, r)
		return
	}

	// HTTP reverse proxy
	target, _ := url.Parse("http://127.0.0.1:" + gatewayPort)
	proxy := httputil.NewSingleHostReverseProxy(target)
	defaultDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		defaultDirector(req)
		// Force plain HTML so we can inject a tiny compatibility patch.
		req.Header.Del("Accept-Encoding")
	}
	proxy.ModifyResponse = injectControlUICustomizations
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("Proxy error: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"gateway unavailable"}`))
	}
	proxy.ServeHTTP(w, r)
}

func injectControlUICustomizations(resp *http.Response) error {
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if !strings.Contains(contentType, "text/html") {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	resp.Body.Close()

	html := string(body)
	if strings.Contains(html, `id="openclaw-render-tool-card-override"`) {
		resp.Body = io.NopCloser(strings.NewReader(html))
		resp.ContentLength = int64(len(html))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(html)))
		return nil
	}

	lowerHTML := strings.ToLower(html)
	headIndex := strings.Index(lowerHTML, "</head>")
	if headIndex < 0 {
		resp.Body = io.NopCloser(strings.NewReader(html))
		resp.ContentLength = int64(len(html))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(html)))
		return nil
	}

	updated := html[:headIndex] + controlUICustomizations + html[headIndex:]
	resp.Body = io.NopCloser(strings.NewReader(updated))
	resp.ContentLength = int64(len(updated))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(updated)))
	resp.Header.Del("Content-Encoding")
	return nil
}

func proxyWebSocket(w http.ResponseWriter, r *http.Request) {
	backend, err := net.Dial("tcp", "127.0.0.1:"+gatewayPort)
	if err != nil {
		http.Error(w, "Gateway unavailable", http.StatusBadGateway)
		return
	}
	defer backend.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer client.Close()

	// Forward the original request
	if err := r.Write(backend); err != nil {
		log.Printf("WebSocket forward error: %v", err)
		return
	}

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		io.Copy(backend, client)
		if tc, ok := backend.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		wg.Done()
	}()
	go func() {
		io.Copy(client, backend)
		wg.Done()
	}()
	wg.Wait()
}
