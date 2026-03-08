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

const controlUIScriptPath = "/__openclaw_render_ui.js"

const controlUICustomizations = `<style id="openclaw-render-tool-card-override">
.chat-group.tool {
	display: none !important;
}
.chat-tool-card {
	display: none !important;
}
.chat-group.assistant:has(.chat-tool-card):not(:has(.chat-text)):not(:has(.chat-message-images)):not(:has(.chat-thinking)),
.chat-group.other:has(.chat-tool-card):not(:has(.chat-text)):not(:has(.chat-message-images)):not(:has(.chat-thinking)) {
	display: none !important;
}
.chat-queue.oc-queue-active {
	border-color: rgba(255, 92, 92, 0.22) !important;
	background: rgba(255, 92, 92, 0.05) !important;
}
.oc-queue-note {
	margin-top: 8px;
	font-size: 12px;
	line-height: 1.45;
	color: rgba(34, 34, 34, 0.72);
}
</style>
<script defer src="` + controlUIScriptPath + `" id="openclaw-render-tool-card-script"></script>`

const controlUIScript = `(() => {
  const settingsKey = "openclaw.control.settings.v1";
  const className = "oc-hide-tool-cards";
  const webchatEchoSender = "openclaw-control-ui";
  const sessionResetPromptPrefix = "A new session was started via /new or /reset.";

  function shouldHideToolCards() {
    try {
      const raw = window.localStorage.getItem(settingsKey);
      const parsed = raw ? JSON.parse(raw) : null;
      return Boolean(parsed && parsed.chatShowThinking === false);
    } catch {
      return false;
    }
  }

  function setDisplay(el, hide) {
    if (!el) {
      return;
    }
    if (hide) {
      el.style.setProperty("display", "none", "important");
      return;
    }
    el.style.removeProperty("display");
  }

  function normalizeText(value) {
    return String(value || "")
      .replace(/\s+/g, " ")
      .trim();
  }

  function extractGroupText(group) {
    return normalizeText(
      Array.from(group.querySelectorAll(".chat-text"))
        .map((el) => el.textContent || "")
        .join("\n"),
    );
  }

  function extractGroupTimestamp(group) {
    return normalizeText(group.querySelector(".chat-group-timestamp")?.textContent || "");
  }

  function isSessionResetPromptText(value) {
    return normalizeText(value).toLowerCase().startsWith(sessionResetPromptPrefix.toLowerCase());
  }

  function setButtonLabel(button, label) {
    if (!(button instanceof HTMLElement)) {
      return;
    }
    const desired = String(label || "");
    const textNodes = Array.from(button.childNodes).filter((node) => node.nodeType === Node.TEXT_NODE);
    const primaryText = textNodes[0] || null;
    if (primaryText) {
      primaryText.textContent = desired;
      for (let i = 1; i < textNodes.length; i += 1) {
        textNodes[i].textContent = "";
      }
      return;
    }
    button.insertBefore(document.createTextNode(desired), button.firstChild);
  }

  function syncQueuedState() {
    const compose = document.querySelector(".chat-compose");
    if (!(compose instanceof HTMLElement)) {
      return;
    }

    const actionButtons = compose.querySelectorAll(".chat-compose__actions .btn");
    const secondaryButton = actionButtons[0];
    const primaryButton = compose.querySelector(".chat-compose__actions .btn.primary");
    const runActive = normalizeText(secondaryButton?.textContent || "").toLowerCase().startsWith("stop");
    const queue = document.querySelector(".chat-queue");
    const queuedItems = document.querySelectorAll(".chat-queue__item").length;

    setButtonLabel(primaryButton, runActive ? "Queue" : "Send");

    if (primaryButton instanceof HTMLElement) {
      primaryButton.title = runActive
        ? "Current reply is still running. New messages will be queued automatically."
        : "";
      primaryButton.dataset.ocAction = runActive ? "queue" : "send";
    }

    if (queue instanceof HTMLElement) {
      queue.classList.toggle("oc-queue-active", runActive || queuedItems > 0);
      let note = queue.querySelector(".oc-queue-note");
      if (!(note instanceof HTMLElement)) {
        note = document.createElement("div");
        note.className = "oc-queue-note";
        queue.appendChild(note);
      }
      if (runActive || queuedItems > 0) {
        note.textContent =
          "Current reply is still running. Queued messages will send automatically when it finishes.";
        note.hidden = false;
      } else {
        note.hidden = true;
      }
    }
  }

  function normalizeWebchatEchoGroups() {
    document.querySelectorAll(".chat-group").forEach((group) => {
      const sender = group.querySelector(".chat-sender-name");
      const senderText = normalizeText(sender?.textContent || "").toLowerCase();
      if (senderText !== webchatEchoSender) {
        return;
      }

      group.dataset.ocWebchatEcho = "1";
      if (sender) {
        sender.textContent = "You";
      }

      group.classList.remove("assistant", "other", "tool");
      group.classList.add("user");

      const avatar = group.querySelector(".chat-avatar");
      if (avatar) {
        avatar.classList.remove("assistant", "other", "tool");
        avatar.classList.add("user");
        if (!avatar.querySelector("img")) {
          avatar.textContent = "U";
        }
      }
    });
  }

  function dedupeWebchatEchoGroups() {
    const groups = Array.from(document.querySelectorAll(".chat-group"));
    for (let i = 0; i < groups.length - 1; i += 1) {
      const current = groups[i];
      const next = groups[i + 1];
      if (!(current instanceof HTMLElement) || !(next instanceof HTMLElement)) {
        continue;
      }

      const currentIsEcho = current.dataset.ocWebchatEcho === "1";
      const nextIsEcho = next.dataset.ocWebchatEcho === "1";
      if (!currentIsEcho && !nextIsEcho) {
        continue;
      }

      if (!current.classList.contains("user") || !next.classList.contains("user")) {
        continue;
      }

      const currentText = extractGroupText(current);
      const nextText = extractGroupText(next);
      if (!currentText || currentText !== nextText) {
        continue;
      }

      const currentTs = extractGroupTimestamp(current);
      const nextTs = extractGroupTimestamp(next);
      if (currentTs && nextTs && currentTs !== nextTs) {
        continue;
      }

      if (currentIsEcho && !nextIsEcho) {
        setDisplay(current, true);
        continue;
      }
      if (!currentIsEcho && nextIsEcho) {
        setDisplay(next, true);
        continue;
      }
    }
  }

  function hideInternalResetPromptGroups() {
    document.querySelectorAll(".chat-group").forEach((group) => {
      if (!(group instanceof HTMLElement)) {
        return;
      }
      const hide = isSessionResetPromptText(extractGroupText(group));
      if (hide) {
        group.dataset.ocSessionResetPrompt = "1";
      } else {
        delete group.dataset.ocSessionResetPrompt;
      }
      setDisplay(group, hide);
    });
  }

  function interceptNewSessionWithDraft(event) {
    if (!(event.target instanceof Element)) {
      return;
    }
    const button = event.target.closest(".chat-compose__actions .btn");
    if (!(button instanceof HTMLElement)) {
      return;
    }
    const label = normalizeText(button.textContent || "").toLowerCase();
    if (!label.startsWith("new session")) {
      return;
    }

    const compose = button.closest(".chat-compose");
    if (!(compose instanceof HTMLElement)) {
      return;
    }
    const textarea = compose.querySelector("textarea");
    const sendButton = compose.querySelector(".chat-compose__actions .btn.primary");
    if (!(textarea instanceof HTMLTextAreaElement) || !(sendButton instanceof HTMLElement)) {
      return;
    }

    const draft = textarea.value.trim();
    if (!draft) {
      return;
    }

    event.preventDefault();
    event.stopImmediatePropagation();
    textarea.value = "/new " + draft;
    textarea.dispatchEvent(new Event("input", { bubbles: true }));
    window.setTimeout(() => sendButton.click(), 0);
  }

  function syncToolCardVisibility() {
    const hide = shouldHideToolCards();
    document.documentElement.classList.toggle(className, hide);
    normalizeWebchatEchoGroups();
    dedupeWebchatEchoGroups();
    hideInternalResetPromptGroups();
    syncQueuedState();

    document.querySelectorAll(".chat-tool-card").forEach((card) => {
      setDisplay(card, hide);
      const bubble = card.closest(".chat-bubble");
      if (bubble) {
        const hasContent = bubble.querySelector(".chat-text, .chat-message-images, .chat-thinking");
        if (!hasContent) {
          setDisplay(bubble, hide);
        } else if (!hide) {
          setDisplay(bubble, false);
        }
      }
    });

    document.querySelectorAll(".chat-group.tool").forEach((group) => {
      setDisplay(group, hide);
    });

    document.querySelectorAll(".chat-group.assistant").forEach((group) => {
      const hasToolCard = group.querySelector(".chat-tool-card");
      const hasContent = group.querySelector(".chat-text, .chat-message-images, .chat-thinking");
      if (hasToolCard && !hasContent) {
        setDisplay(group, hide);
      } else if (!hide) {
        setDisplay(group, false);
      }
    });
  }

  const observer = new MutationObserver(() => syncToolCardVisibility());

  function boot() {
    syncToolCardVisibility();
    observer.observe(document.body, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ["class"],
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot, { once: true });
  } else {
    boot();
  }

  window.addEventListener("storage", syncToolCardVisibility);
  window.addEventListener("focus", syncToolCardVisibility);
  document.addEventListener("click", () => window.setTimeout(syncToolCardVisibility, 0), true);
  document.addEventListener("click", interceptNewSessionWithDraft, true);
  window.addEventListener("pageshow", syncToolCardVisibility);
})();`

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
	mux.HandleFunc(controlUIScriptPath, handleControlUIScript)
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
		{"config", "set", "channels.telegram.replyToMode", "off"},
		{"config", "set", "channels.telegram.streaming", "off"},
		{"config", "set", "channels.telegram.blockStreaming", "false"},
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
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Input []string `json:"input,omitempty"`
}

type cpaBootstrapSettings struct {
	envConfigured bool
	createProvider bool
	baseURL       string
	apiKey        string
	apiKeys       []string
	api           string
	models        []cpaModelEntry
	primaryModel  string
	modelFallbacks []string
	imageModel    string
	coderModel    string
}

var defaultCpaModels = []cpaModelEntry{
	{ID: "gpt-5.4", Name: "ChatGPT 5.4", Input: []string{"text", "image"}},
	{ID: "gpt-5", Name: "ChatGPT 5", Input: []string{"text", "image"}},
	{ID: "gpt-5-codex", Name: "GPT-5 Codex", Input: []string{"text", "image"}},
	{ID: "gemini-3-flash", Name: "Gemini 3 Flash", Input: []string{"text", "image"}},
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
	}
	providerIDs := cpaProviderIDs(settings)
	for idx, providerID := range providerIDs {
		providerCfg := cpa
		if providerID != "cpa" {
			providerRaw, exists := providers[providerID]
			if exists {
				providerCfg, ok = providerRaw.(map[string]any)
				if !ok {
					log.Printf("Skipping CPA bootstrap, models.providers.%s is invalid", providerID)
					return
				}
			} else {
				providerCfg = map[string]any{}
				providers[providerID] = providerCfg
				changed = true
			}
		}
		if settings.envConfigured {
			if applyCpaProviderSettings(providerCfg, settings, cpaProviderAPIKey(settings, idx)) {
				changed = true
			}
		}
		if nextModels, modelChanged := mergeCpaModels(providerCfg["models"], settings.models, !settings.envConfigured); modelChanged {
			providerCfg["models"] = nextModels
			changed = true
		}
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
	desiredPrimaryRefs := cpaModelRefChain(providerIDs, append([]string{settings.primaryModel}, settings.modelFallbacks...))
	if ensureManagedAgentModel(modelCfg, desiredPrimaryRefs) {
		changed = true
	}
	if ensureImageModel(defaults, cpaModelRefs(providerIDs, settings.imageModel)) {
		changed = true
	}
	if ensureImageUnderstanding(cfg, settings.imageModel) {
		changed = true
	}

	if ensureModelAliases(defaults, buildCpaAliases(settings.models)) {
		changed = true
	}
	if ensureCoderModel(agents, cpaModelRefs(providerIDs, settings.coderModel)) {
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
	modelFallbacks := parseDelimitedStrings(os.Getenv("CPA_MODEL_FALLBACKS"))
	imageModel := strings.TrimSpace(os.Getenv("CPA_IMAGE_MODEL"))
	coderModel := strings.TrimSpace(os.Getenv("CPA_CODER_MODEL"))
	apiKeys := uniqueNonEmptyStrings(append(
		[]string{strings.TrimSpace(os.Getenv("CPA_API_KEY"))},
		parseDelimitedStrings(os.Getenv("CPA_API_KEYS"))...,
	))

	settings := cpaBootstrapSettings{
		baseURL:      strings.TrimSpace(os.Getenv("CPA_BASE_URL")),
		apiKeys:      apiKeys,
		api:          strings.TrimSpace(os.Getenv("CPA_API")),
		models:       copyCpaModels(defaultCpaModels),
		primaryModel: "gpt-5.4",
		modelFallbacks: nil,
		imageModel:   "gpt-5.4",
		coderModel:   "gpt-5-codex",
	}
	if len(settings.apiKeys) > 0 {
		settings.apiKey = settings.apiKeys[0]
	}

	if parsedModels, ok := parseCpaModels(rawModels); ok {
		settings.models = parsedModels
	}
	if defaultModel != "" {
		settings.primaryModel = defaultModel
	}
	settings.models = appendCpaModel(settings.models, settings.primaryModel)
	settings.modelFallbacks = sanitizeCpaFallbacks(modelFallbacks, settings.primaryModel)
	if len(settings.modelFallbacks) == 0 {
		settings.modelFallbacks = defaultCpaModelFallbacks(settings.models, settings.primaryModel)
	}
	for _, fallback := range settings.modelFallbacks {
		settings.models = appendCpaModel(settings.models, fallback)
	}
	if imageModel != "" {
		settings.imageModel = imageModel
	} else {
		settings.imageModel = settings.primaryModel
	}
	settings.models = appendCpaModel(settings.models, settings.imageModel)

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
	settings.createProvider = settings.baseURL != "" || len(settings.apiKeys) > 0 || rawModels != ""
	settings.envConfigured = settings.createProvider || settings.api != "" || defaultModel != "" || imageModel != "" || coderModel != ""

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
		input := sanitizeCpaModelInput(model.Input)
		if len(input) == 0 {
			input = cpaDefaultInput(id)
		}
		sanitized = append(sanitized, cpaModelEntry{ID: id, Name: name, Input: input})
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
	return append(models, cpaModelEntry{ID: id, Name: cpaDisplayName(id), Input: cpaDefaultInput(id)})
}

func sanitizeCpaModelInput(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(input))
	seen := map[string]struct{}{}
	for _, item := range input {
		value := strings.ToLower(strings.TrimSpace(item))
		if value != "text" && value != "image" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		normalized = append(normalized, value)
		seen[value] = struct{}{}
	}
	if len(normalized) == 0 {
		return nil
	}
	if _, exists := seen["text"]; !exists {
		normalized = append([]string{"text"}, normalized...)
	}
	return normalized
}

func cpaDefaultInput(id string) []string {
	if cpaLikelySupportsVision(id) {
		return []string{"text", "image"}
	}
	return []string{"text"}
}

func cpaLikelySupportsVision(id string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))
	switch {
	case lower == "":
		return false
	case strings.Contains(lower, "vision"):
		return true
	case strings.Contains(lower, "vl"):
		return true
	case strings.Contains(lower, "gemini"):
		return true
	case strings.Contains(lower, "gpt-4o"):
		return true
	case strings.Contains(lower, "gpt-5"):
		return true
	case strings.Contains(lower, "chatgpt-5"):
		return true
	case strings.Contains(lower, "claude"):
		return true
	case strings.Contains(lower, "grok"):
		return true
	case strings.Contains(lower, "kimi"):
		return true
	case strings.Contains(lower, "glm-4.6v"):
		return true
	default:
		return false
	}
}

func applyCpaProviderSettings(cpa map[string]any, settings cpaBootstrapSettings, apiKey string) bool {
	changed := false
	if settings.baseURL != "" {
		if current, _ := cpa["baseUrl"].(string); current != settings.baseURL {
			cpa["baseUrl"] = settings.baseURL
			changed = true
		}
	}
	if apiKey != "" {
		if current, _ := cpa["apiKey"].(string); current != apiKey {
			cpa["apiKey"] = apiKey
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

func cpaProviderIDs(settings cpaBootstrapSettings) []string {
	count := len(settings.apiKeys)
	if count <= 1 {
		return []string{"cpa"}
	}
	ids := make([]string, 0, count)
	for idx := 0; idx < count; idx++ {
		if idx == 0 {
			ids = append(ids, "cpa")
			continue
		}
		ids = append(ids, fmt.Sprintf("cpa%d", idx+1))
	}
	return ids
}

func cpaProviderAPIKey(settings cpaBootstrapSettings, idx int) string {
	if idx >= 0 && idx < len(settings.apiKeys) {
		return settings.apiKeys[idx]
	}
	return settings.apiKey
}

func cpaModelRefs(providerIDs []string, modelID string) []string {
	refs := make([]string, 0, len(providerIDs))
	for _, providerID := range providerIDs {
		providerID = strings.TrimSpace(providerID)
		if providerID == "" {
			continue
		}
		refs = append(refs, providerID+"/"+modelID)
	}
	return refs
}

func cpaModelRefChain(providerIDs []string, modelIDs []string) []string {
	refs := make([]string, 0, len(providerIDs)*len(modelIDs))
	for _, modelID := range modelIDs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		refs = append(refs, cpaModelRefs(providerIDs, modelID)...)
	}
	return refs
}

func sanitizeCpaFallbacks(source []string, primary string) []string {
	out := make([]string, 0, len(source))
	seen := map[string]struct{}{}
	primaryKey := strings.ToLower(strings.TrimSpace(primary))
	if primaryKey != "" {
		seen[primaryKey] = struct{}{}
	}
	for _, item := range source {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		key := strings.ToLower(item)
		if _, exists := seen[key]; exists {
			continue
		}
		out = append(out, item)
		seen[key] = struct{}{}
	}
	return out
}

func defaultCpaModelFallbacks(models []cpaModelEntry, primary string) []string {
	candidates := []string{"gemini-3-flash", "gpt-5", "gpt-5-codex"}
	available := map[string]struct{}{}
	for _, model := range models {
		available[strings.ToLower(strings.TrimSpace(model.ID))] = struct{}{}
	}
	out := make([]string, 0, len(candidates))
	primaryKey := strings.ToLower(strings.TrimSpace(primary))
	for _, candidate := range candidates {
		key := strings.ToLower(candidate)
		if key == primaryKey {
			continue
		}
		if _, exists := available[key]; !exists {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func mergeCpaModels(raw any, desired []cpaModelEntry, keepExtras bool) ([]map[string]any, bool) {
	extras := []map[string]any{}
	existingEntries := map[string]map[string]any{}
	existingNames := map[string]string{}
	existingInputs := map[string][]string{}
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
			existingEntries[id] = entry
			existingNames[id] = strings.TrimSpace(name)
			existingInputs[id] = normalizeExistingModelInput(entry["input"])
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
		next = append(next, buildCpaModelMap(model, existingEntries[model.ID]))
		if _, exists := existingIDs[model.ID]; !exists {
			changed = true
			continue
		}
		if currentName := existingNames[model.ID]; currentName != model.Name {
			changed = true
		}
		if !stringSlicesEqual(existingInputs[model.ID], model.Input) {
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

func ensureManagedAgentModel(target map[string]any, desiredRefs []string) bool {
	if len(desiredRefs) == 0 {
		return false
	}

	currentPrimary, currentFallbacks, ok := coerceManagedAgentModel(target)
	if !ok {
		return false
	}
	if currentPrimary != "" && !strings.HasPrefix(currentPrimary, "cpa") {
		return false
	}
	for _, fallback := range currentFallbacks {
		if fallback != "" && !strings.HasPrefix(fallback, "cpa") {
			return false
		}
	}

	changed := false
	if currentPrimary != desiredRefs[0] {
		target["primary"] = desiredRefs[0]
		changed = true
	}
	desiredFallbacks := desiredRefs[1:]
	if !stringSlicesEqual(currentFallbacks, desiredFallbacks) {
		target["fallbacks"] = toAnyStrings(desiredFallbacks)
		changed = true
	}
	return changed
}

func ensureImageModel(defaults map[string]any, desiredRefs []string) bool {
	currentRaw, exists := defaults["imageModel"]
	if exists {
		if current, ok := currentRaw.(string); ok {
			current = strings.TrimSpace(current)
			if current != "" && !strings.HasPrefix(current, "cpa/") {
				return false
			}
			defaults["imageModel"] = map[string]any{"primary": current}
		}
	}

	imageModel, ok := ensureObject(defaults, "imageModel")
	if !ok {
		return false
	}
	return ensureManagedAgentModel(imageModel, desiredRefs)
}

func ensureImageUnderstanding(cfg map[string]any, desiredModel string) bool {
	tools, ok := ensureObject(cfg, "tools")
	if !ok {
		return false
	}
	media, ok := ensureObject(tools, "media")
	if !ok {
		return false
	}
	image, ok := ensureObject(media, "image")
	if !ok {
		return false
	}

	changed := false
	if enabled, ok := image["enabled"].(bool); !ok || !enabled {
		image["enabled"] = true
		changed = true
	}

	modelsRaw, exists := image["models"]
	switch typed := modelsRaw.(type) {
	case nil:
		image["models"] = []map[string]any{{"provider": "cpa", "model": desiredModel}}
		return true
	case []any:
		if len(typed) == 0 {
			image["models"] = []map[string]any{{"provider": "cpa", "model": desiredModel}}
			return true
		}
	default:
		if exists {
			image["models"] = []map[string]any{{"provider": "cpa", "model": desiredModel}}
			return true
		}
	}

	return changed
}

func buildCpaModelMap(model cpaModelEntry, existing map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range existing {
		out[key] = value
	}
	out["id"] = model.ID
	out["name"] = model.Name
	if len(model.Input) > 0 {
		input := make([]any, 0, len(model.Input))
		for _, item := range model.Input {
			input = append(input, item)
		}
		out["input"] = input
	}
	return out
}

func normalizeExistingModelInput(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	input := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok {
			continue
		}
		input = append(input, value)
	}
	return sanitizeCpaModelInput(input)
}

func stringSlicesEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for idx := range a {
		if a[idx] != b[idx] {
			return false
		}
	}
	return true
}

func coerceManagedAgentModel(target map[string]any) (string, []string, bool) {
	currentRaw, exists := target["primary"]
	if !exists {
		return "", nil, true
	}
	current, ok := currentRaw.(string)
	if !ok {
		return "", nil, false
	}
	return strings.TrimSpace(current), readStringSlice(target["fallbacks"]), true
}

func readStringSlice(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return values
}

func toAnyStrings(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func parseDelimitedStrings(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	})
	values := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		values = append(values, field)
	}
	return values
}

func uniqueNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		out = append(out, value)
		seen[value] = struct{}{}
	}
	return out
}

func ensureCoderModel(agents map[string]any, desiredRefs []string) bool {
	if len(desiredRefs) == 0 {
		return false
	}
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
		switch current := agent["model"].(type) {
		case string:
			current = strings.TrimSpace(current)
			if current != "" && !strings.HasPrefix(current, "cpa/") {
				continue
			}
			if len(desiredRefs) == 1 {
				if current != desiredRefs[0] {
					agent["model"] = desiredRefs[0]
					changed = true
				}
			} else {
				agent["model"] = map[string]any{
					"primary":   desiredRefs[0],
					"fallbacks": toAnyStrings(desiredRefs[1:]),
				}
				changed = true
			}
		case map[string]any:
			if ensureManagedAgentModel(current, desiredRefs) {
				changed = true
			}
		case nil:
			if len(desiredRefs) == 1 {
				agent["model"] = desiredRefs[0]
			} else {
				agent["model"] = map[string]any{
					"primary":   desiredRefs[0],
					"fallbacks": toAnyStrings(desiredRefs[1:]),
				}
			}
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

func handleControlUIScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(controlUIScript))
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
