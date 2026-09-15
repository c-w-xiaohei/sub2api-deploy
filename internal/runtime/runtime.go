package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const HostStateVersion = 1

var siteIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var domainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// These package-private seams keep the command API fixed while allowing the
// HTTP and stale-lock boundaries to be exercised without external services.
var neonAPIBaseURL = "https://console.neon.tech/api/v2"
var neonHTTPClient = &http.Client{Timeout: 30 * time.Second}
var neonProcessProbe = func(pid int) error { return syscall.Kill(pid, 0) }

type DeployState struct {
	ActiveSlot    string `json:"activeSlot"`
	PreviousSlot  string `json:"previousSlot,omitempty"`
	ActiveImage   string `json:"activeImage"`
	PreviousImage string `json:"previousImage,omitempty"`
	PostgresMode  string `json:"postgresMode,omitempty"`
	RedisMode     string `json:"redisMode,omitempty"`
}

type LegacyCode2Layout struct {
	RuntimeRoot      string `json:"runtimeRoot"`
	ComposeProject   string `json:"composeProject"`
	RouteLayout      string `json:"routeLayout"`
	HandoverComplete bool   `json:"handoverComplete"`
}

type HostState struct {
	Version     int                `json:"version"`
	Sites       []string           `json:"sites"`
	LegacyCode2 *LegacyCode2Layout `json:"legacyCode2,omitempty"`
}

func atomicWrite(path string, contents []byte, prefix string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+prefix+".*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporaryName, 0o600); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func stringValue(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case bool:
		return strconv.FormatBool(value)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return fmt.Sprint(value)
	}
}

func RenderDotenv(values map[string]any) (string, error) {
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if !envKeyPattern.MatchString(key) {
			return "", fmt.Errorf("invalid environment key: %s", key)
		}
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok {
			if strings.ContainsRune(text, 0) {
				return "", fmt.Errorf("%s contains NUL", key)
			}
			if strings.ContainsAny(text, "\r\n") {
				return "", fmt.Errorf("%s contains newline; multiline secrets are unsupported", key)
			}
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var result strings.Builder
	for _, key := range keys {
		value := strings.ReplaceAll(stringValue(values[key]), `\`, `\\`)
		value = strings.ReplaceAll(value, `"`, `\"`)
		fmt.Fprintf(&result, "%s=\"%s\"\n", key, value)
	}
	return result.String(), nil
}

func WriteRuntimeEnv(path string, values map[string]any, slot, slotDataDir string, autoSetupFalse bool) error {
	if slot != "" {
		values["SLOT"] = slot
	}
	if slotDataDir != "" {
		values["SLOT_DATA_DIR"] = slotDataDir
	}
	if autoSetupFalse {
		values["AUTO_SETUP"] = "false"
	}
	contents, err := RenderDotenv(values)
	if err != nil {
		return err
	}
	return atomicWrite(path, []byte(contents), "runtime-env")
}

func WriteAppEnv(path string, values map[string]any) error {
	for key, value := range values {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", key)
		}
	}
	contents, err := RenderDotenv(values)
	if err != nil {
		return err
	}
	return atomicWrite(path, []byte(strings.ReplaceAll(contents, "$", "$$")), "app-env")
}

func ReadRuntimeEnv(path, key string) (string, error) {
	if !envKeyPattern.MatchString(key) {
		return "", errors.New("invalid environment key")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, key+"=") {
			continue
		}
		value := strings.TrimPrefix(line, key+"=")
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			var decoded string
			if err := json.Unmarshal([]byte(value), &decoded); err != nil {
				return "", err
			}
			return decoded, nil
		}
		return value, nil
	}
	return "", os.ErrNotExist
}

func validateDeployState(state DeployState) error {
	if state.ActiveSlot != "blue" && state.ActiveSlot != "green" {
		return errors.New("deployment state activeSlot is invalid")
	}
	if state.PreviousSlot != "" && state.PreviousSlot != "blue" && state.PreviousSlot != "green" {
		return errors.New("deployment state previousSlot is invalid")
	}
	if state.PostgresMode != "" && state.PostgresMode != "docker" && state.PostgresMode != "neon" {
		return errors.New("deployment state postgresMode is invalid")
	}
	if state.RedisMode != "" && state.RedisMode != "docker" && state.RedisMode != "upstash" {
		return errors.New("deployment state redisMode is invalid")
	}
	return nil
}

func decodeDeployState(data []byte) (DeployState, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return DeployState{}, err
	}
	allowed := map[string]bool{"activeSlot": true, "previousSlot": true, "activeImage": true, "previousImage": true, "postgresMode": true, "redisMode": true}
	for key := range raw {
		if !allowed[key] {
			return DeployState{}, errors.New("deployment state contains a credential or unsupported field")
		}
	}
	var state DeployState
	if err := json.Unmarshal(data, &state); err != nil {
		return DeployState{}, err
	}
	if err := validateDeployState(state); err != nil {
		return DeployState{}, err
	}
	return state, nil
}

func ReadDeployState(path string) (DeployState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DeployState{}, err
	}
	return decodeDeployState(data)
}

func ReadStateField(path, field string) (string, error) {
	state, err := ReadDeployState(path)
	if err != nil {
		return "", err
	}
	fields := map[string]string{
		"activeSlot": state.ActiveSlot, "previousSlot": state.PreviousSlot,
		"activeImage": state.ActiveImage, "previousImage": state.PreviousImage,
		"postgresMode": state.PostgresMode, "redisMode": state.RedisMode,
	}
	value, ok := fields[field]
	if !ok || value == "" {
		return "", fmt.Errorf("deployment state field %s is missing", field)
	}
	return value, nil
}

func ReadJSONField(path, field string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return "", err
	}
	result, ok := value[field].(string)
	if !ok || result == "" {
		return "", fmt.Errorf("JSON field %s is missing", field)
	}
	return result, nil
}

func ReadJSONFieldInput(input []byte, field string) (string, error) {
	var value map[string]any
	if err := json.Unmarshal(input, &value); err != nil || value == nil {
		return "", errors.New("JSON input must be an object")
	}
	result, ok := value[field].(string)
	if !ok || result == "" {
		return "", fmt.Errorf("JSON field %s is missing", field)
	}
	return result, nil
}

func HasPersistedModes(path string) (bool, error) {
	state, err := ReadDeployState(path)
	if err != nil {
		return false, err
	}
	return state.PostgresMode != "" && state.RedisMode != "", nil
}

func CheckMappingState(path, want string) error {
	if want != "pending" && want != "complete" && want != "either" {
		return errors.New("requested mapping state must be pending, complete, or either")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	state, err := ParseHostState(data)
	if err != nil {
		return err
	}
	if state.Version != HostStateVersion || len(state.Sites) != 1 || state.Sites[0] != "code2" || state.LegacyCode2 == nil || state.LegacyCode2.RuntimeRoot != "runtime" || state.LegacyCode2.ComposeProject != "sub2api" || state.LegacyCode2.RouteLayout != "flat" {
		return errors.New("host state does not match the legacy code2 mapping")
	}
	if want != "either" && state.LegacyCode2.HandoverComplete != (want == "complete") {
		return errors.New("host state handover status does not match the requested mapping")
	}
	return nil
}

func EdgeEnv(output io.Writer) error {
	values := map[string]string{
		"TRAEFIK_IMAGE": os.Getenv("TRAEFIK_IMAGE"), "ACME_EMAIL": os.Getenv("ACME_EMAIL"),
		"CLOUDFLARE_DNS_API_TOKEN": os.Getenv("CLOUDFLARE_API_TOKEN"), "EDGE_RUNTIME_ROOT": os.Getenv("EDGE_RUNTIME_ROOT"),
	}
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	_, err = output.Write(append(data, '\n'))
	return err
}

func TransitionState(path, activeSlot, activeImage string) (string, error) {
	state, err := ReadDeployState(path)
	if err != nil {
		return "", err
	}
	if activeSlot != "blue" && activeSlot != "green" {
		return "", errors.New("deployment state activeSlot is invalid")
	}
	state.PreviousSlot, state.PreviousImage = state.ActiveSlot, state.ActiveImage
	state.ActiveSlot, state.ActiveImage = activeSlot, activeImage
	data, err := json.Marshal(state)
	return string(data), err
}

func SwapState(path string) (string, error) {
	state, err := ReadDeployState(path)
	if err != nil {
		return "", err
	}
	if state.PreviousSlot == "" || state.PreviousImage == "" {
		return "", errors.New("deployment state has no previous slot and image")
	}
	state.ActiveSlot, state.PreviousSlot = state.PreviousSlot, state.ActiveSlot
	state.ActiveImage, state.PreviousImage = state.PreviousImage, state.ActiveImage
	data, err := json.Marshal(state)
	return string(data), err
}

func WriteDeployState(path string, value map[string]any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	state, err := decodeDeployState(data)
	if err != nil {
		return err
	}
	data, err = json.Marshal(state)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(path, data, "deploy-state")
}

func AssertDeploymentModes(state DeployState, postgres, redis, path string) error {
	if state.PostgresMode == "" || state.RedisMode == "" {
		return fmt.Errorf("deployment state has no persisted postgresMode/redisMode; migration required: verify the existing data placement, then run sub2api-deploy runtime deployment-mode adopt %s %s %s", path, postgres, redis)
	}
	if state.PostgresMode != postgres {
		return fmt.Errorf("postgresMode change from %s to %s requires migration; ordinary pulumi up does not migrate PostgreSQL data", state.PostgresMode, postgres)
	}
	if state.RedisMode != redis {
		return fmt.Errorf("redisMode change from %s to %s requires migration; ordinary pulumi up does not migrate Redis data", state.RedisMode, redis)
	}
	return nil
}

func AdoptDeploymentModes(path, postgres, redis string) error {
	if postgres != "docker" && postgres != "neon" || redis != "docker" && redis != "upstash" {
		return errors.New("adopt requires postgresMode docker|neon and redisMode docker|upstash")
	}
	state, err := ReadDeployState(path)
	if err != nil {
		return err
	}
	if state.PostgresMode != "" || state.RedisMode != "" {
		return errors.New("deployment state already records data modes; use migration instead of adopt")
	}
	return WriteDeployState(path, map[string]any{"activeSlot": state.ActiveSlot, "activeImage": state.ActiveImage, "postgresMode": postgres, "redisMode": redis, "previousSlot": emptyToNil(state.PreviousSlot), "previousImage": emptyToNil(state.PreviousImage)})
}

func emptyToNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func ParseSiteIDs(input string) ([]string, error) {
	parts := strings.Split(input, ",")
	if len(parts) == 0 || (len(parts) == 1 && strings.TrimSpace(parts[0]) == "") {
		return nil, errors.New("configured Site IDs must be a comma-separated list of non-empty Site IDs")
	}
	seen := map[string]bool{}
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
		if !siteIDPattern.MatchString(parts[index]) {
			return nil, fmt.Errorf("invalid Site ID %q", parts[index])
		}
		if parts[index] == "edge" {
			return nil, errors.New(`Site ID "edge" is reserved for the shared Edge`)
		}
		if seen[parts[index]] {
			return nil, fmt.Errorf("duplicate Site ID %q", parts[index])
		}
		seen[parts[index]] = true
	}
	sort.Strings(parts)
	return parts, nil
}

func ParseHostState(data []byte) (HostState, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return HostState{}, errors.New("host state is not valid JSON; inspect and repair it before proceeding")
	}
	for key := range raw {
		if key != "version" && key != "sites" && key != "legacyCode2" {
			return HostState{}, errors.New("host state contains a credential or unsupported field; only non-secret Site registry metadata is allowed")
		}
	}
	var state HostState
	if err := json.Unmarshal(data, &state); err != nil || state.Version != HostStateVersion {
		return HostState{}, errors.New("host state version is unsupported")
	}
	if state.Sites == nil {
		return HostState{}, errors.New("host state is malformed: sites must be an array of Site ID strings")
	}
	for _, site := range state.Sites {
		if !siteIDPattern.MatchString(site) {
			return HostState{}, fmt.Errorf("invalid Site ID %q", site)
		}
	}
	if len(unique(state.Sites)) != len(state.Sites) {
		return HostState{}, errors.New("duplicate Site ID")
	}
	if legacyValue, hasLegacy := raw["legacyCode2"]; hasLegacy {
		var legacyRaw map[string]json.RawMessage
		if err := json.Unmarshal(legacyValue, &legacyRaw); err != nil || legacyRaw == nil || len(legacyRaw) != 4 {
			return HostState{}, errors.New("host state legacy mapping contains a credential or unsupported field")
		}
		for key := range legacyRaw {
			if key != "runtimeRoot" && key != "composeProject" && key != "routeLayout" && key != "handoverComplete" {
				return HostState{}, errors.New("host state legacy mapping contains a credential or unsupported field")
			}
		}
		for _, key := range []string{"runtimeRoot", "composeProject", "routeLayout"} {
			var value string
			if err := json.Unmarshal(legacyRaw[key], &value); err != nil {
				return HostState{}, errors.New("host state legacy mapping fields have invalid JSON types")
			}
		}
		var handover bool
		if err := json.Unmarshal(legacyRaw["handoverComplete"], &handover); err != nil || string(legacyRaw["handoverComplete"]) == "null" {
			return HostState{}, errors.New("host state legacy mapping fields have invalid JSON types")
		}
	}
	if state.LegacyCode2 != nil && (state.LegacyCode2.RuntimeRoot != "runtime" || state.LegacyCode2.ComposeProject != "sub2api" || state.LegacyCode2.RouteLayout != "flat" || !contains(state.Sites, "code2")) {
		return HostState{}, errors.New("host state legacy mapping must describe the code2 runtime/sub2api flat layout")
	}
	sort.Strings(state.Sites)
	return state, nil
}

func unique(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func WriteHostState(path, siteList string, legacyHandover string) error {
	sites, err := ParseSiteIDs(siteList)
	if err != nil {
		return err
	}
	if len(sites) == 0 {
		return errors.New("host state requires at least one Site ID; removing the last Site requires the explicit retirement workflow")
	}
	var legacy *LegacyCode2Layout
	if legacyHandover != "" {
		if len(sites) != 1 || sites[0] != "code2" || (legacyHandover != "pending" && legacyHandover != "complete") {
			return errors.New("write-legacy requires only code2 and pending or complete handover state")
		}
		legacy = &LegacyCode2Layout{RuntimeRoot: "runtime", ComposeProject: "sub2api", RouteLayout: "flat", HandoverComplete: legacyHandover == "complete"}
	} else if data, readErr := os.ReadFile(path); readErr == nil {
		old, parseErr := ParseHostState(data)
		if parseErr != nil {
			return parseErr
		}
		legacy = old.LegacyCode2
	}
	state := HostState{Version: HostStateVersion, Sites: sites, LegacyCode2: legacy}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'), "host-state")
}

func ReadHostState(path string) (HostState, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return HostState{}, false, nil
	}
	if err != nil {
		return HostState{}, false, err
	}
	state, err := ParseHostState(data)
	return state, true, err
}

func CheckHostPreflight(configured, statePath, pending, expected string) error {
	want, err := ParseSiteIDs(configured)
	if err != nil {
		return err
	}
	if len(want) == 0 {
		return errors.New("host preflight requires at least one configured Site ID")
	}
	if expected != "" {
		var rawModes map[string]json.RawMessage
		if err := json.Unmarshal([]byte(expected), &rawModes); err != nil || rawModes == nil {
			return errors.New("expected Site modes are not valid JSON")
		}
		if len(rawModes) != len(want) {
			return errors.New("expected Site modes do not match configured Sites")
		}
		for site, rawMode := range rawModes {
			if !contains(want, site) {
				return fmt.Errorf("expected Site modes do not match configured Sites: unknown Site %s", site)
			}
			var mode map[string]json.RawMessage
			if err := json.Unmarshal(rawMode, &mode); err != nil || mode == nil || len(mode) != 2 {
				return fmt.Errorf("Site %s has invalid expected data modes", site)
			}
			for key := range mode {
				if key != "postgresMode" && key != "redisMode" {
					return fmt.Errorf("Site %s has invalid expected data modes", site)
				}
			}
			var parsed struct {
				PostgresMode string `json:"postgresMode"`
				RedisMode    string `json:"redisMode"`
			}
			if err := json.Unmarshal(rawMode, &parsed); err != nil || (parsed.PostgresMode != "docker" && parsed.PostgresMode != "neon") || (parsed.RedisMode != "docker" && parsed.RedisMode != "upstash") {
				return fmt.Errorf("Site %s has invalid expected data modes", site)
			}
		}
	}
	state, exists, err := ReadHostState(statePath)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := os.Stat(filepath.Join(filepath.Dir(statePath), "deploy-state.json")); err == nil {
			if len(want) != 1 || want[0] != "code2" {
				return errors.New("legacy runtime/deploy-state.json requires exactly one configured Site ID: code2")
			}
			return errors.New("legacy code2 runtime detected; run the explicitly approved adopt-single-site-layout.sh maintenance-window procedure before ordinary Pulumi operations")
		}
		return nil
	}
	if expected != "" {
		var modes map[string]struct {
			PostgresMode string `json:"postgresMode"`
			RedisMode    string `json:"redisMode"`
		}
		if err := json.Unmarshal([]byte(expected), &modes); err != nil {
			return errors.New("expected Site modes are not valid JSON")
		}
		if len(modes) != len(want) {
			return errors.New("expected Site modes do not match configured Sites")
		}
		for _, site := range want {
			mode, ok := modes[site]
			if !ok || (mode.PostgresMode != "docker" && mode.PostgresMode != "neon") || (mode.RedisMode != "docker" && mode.RedisMode != "upstash") {
				return fmt.Errorf("Site %s has invalid expected data modes", site)
			}
			isLegacy := state.LegacyCode2 != nil && site == "code2"
			if isLegacy {
				continue
			}
			statePathForSite := filepath.Join(filepath.Dir(statePath), "sites", site, "deploy-state.json")
			deployState, readErr := ReadDeployState(statePathForSite)
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			if readErr != nil {
				return fmt.Errorf("Site %s deploy state is invalid: %w", site, readErr)
			}
			if readErr := AssertDeploymentModes(deployState, mode.PostgresMode, mode.RedisMode, statePathForSite); readErr != nil {
				return readErr
			}
		}
	}
	for _, site := range state.Sites {
		isLegacy := state.LegacyCode2 != nil && site == "code2"
		statePathForSite := filepath.Join(filepath.Dir(statePath), "sites", site, "deploy-state.json")
		if isLegacy {
			statePathForSite = filepath.Join(filepath.Dir(statePath), "deploy-state.json")
		}
		if _, readErr := os.Stat(statePathForSite); readErr != nil {
			return fmt.Errorf("host registry records Site %s but its deploy state is missing; restore it or use the explicit retirement workflow", site)
		}
	}
	for _, site := range state.Sites {
		if !contains(want, site) {
			return fmt.Errorf("host registry records Site ID(s) no longer configured: %s; run the explicit Site retirement workflow before removing the Site key", site)
		}
	}
	if state.LegacyCode2 != nil && !state.LegacyCode2.HandoverComplete && pending != "true" {
		return errors.New("legacy code2 layout is recorded; ordinary Pulumi operations are blocked until the explicitly approved maintenance-window handover completes")
	}
	return nil
}

func ValidateDeploymentPreflight(statePath, markerPath, postgres, redis string) error {
	state, err := ReadDeployState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		if _, markerErr := os.Stat(markerPath); markerErr == nil {
			return errors.New("bootstrap marker exists but deploy-state is missing; restore/adopt state before running pulumi up")
		}
		return nil
	}
	if err != nil {
		return err
	}
	return AssertDeploymentModes(state, postgres, redis, statePath)
}

func RenderSiteRoute(template, siteID, domain, slot, alias string) (string, error) {
	if !siteIDPattern.MatchString(siteID) {
		return "", errors.New("site ID is invalid")
	}
	domain = strings.ToLower(domain)
	if len(domain) > 253 || !domainPattern.MatchString(domain) {
		return "", errors.New("domain is invalid")
	}
	if slot != "blue" && slot != "green" {
		return "", errors.New("slot must be blue or green")
	}
	allowed := []string{"sub2api-" + siteID + "-" + slot}
	if siteID == "code2" {
		allowed = append(allowed, "sub2api-"+slot)
	}
	if !contains(allowed, alias) {
		return "", errors.New("active edge alias does not belong to this Site and slot")
	}
	rendered := strings.NewReplacer("${SITE_ID}", siteID, "${DOMAIN}", domain, "${SLOT}", slot, "${ACTIVE_EDGE_ALIAS}", alias).Replace(template)
	if regexp.MustCompile(`\$\{[A-Z0-9_]+\}`).MatchString(rendered) {
		return "", errors.New("site route template was not fully rendered")
	}
	return rendered, nil
}

func WriteSiteRoute(templatePath, destination, siteID, domain, slot, alias string) error {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return err
	}
	rendered, err := RenderSiteRoute(string(data), siteID, domain, slot, alias)
	if err != nil {
		return err
	}
	return atomicWrite(destination, []byte(rendered), "site-route")
}

func RenderTraefikConfig(template, email string) (string, error) {
	if email == "" || strings.ContainsAny(email, "\r\n\x00") {
		return "", errors.New("acmeEmail contains an unsupported control character")
	}
	rendered := strings.ReplaceAll(template, "${ACME_EMAIL}", email)
	if strings.Contains(rendered, "${ACME_EMAIL}") {
		return "", errors.New("ACME_EMAIL was not rendered")
	}
	return rendered, nil
}

func WriteEdgeConfig(root, staticTemplate, singTemplate, email, server, target string) error {
	if strings.ContainsAny(server, "\r\n\x00") || strings.ContainsAny(target, "\r\n\x00") || server == "" || target == "" {
		return errors.New("sing-box configuration contains an unsupported control character")
	}
	static, err := os.ReadFile(staticTemplate)
	if err != nil {
		return err
	}
	rendered, err := RenderTraefikConfig(string(static), email)
	if err != nil {
		return err
	}
	sing, err := os.ReadFile(singTemplate)
	if err != nil {
		return err
	}
	singRendered := strings.NewReplacer("${SING_BOX_SERVER_NAME}", server, "${SING_BOX_TARGET}", target).Replace(string(sing))
	if regexp.MustCompile(`\$\{[A-Z0-9_]+\}`).MatchString(singRendered) {
		return errors.New("sing-box template was not fully rendered")
	}
	if err := atomicWrite(filepath.Join(root, "traefik.yml"), []byte(rendered), "traefik"); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(root, "dynamic", "00-sing-box.yml"), []byte(singRendered), "sing-box")
}

func WriteBootstrapMarker(path string) error {
	return atomicWrite(path, []byte("sub2api-bootstrap-v1\n"), "bootstrap-marker")
}

func VerifyLegacyAppEnv(path, configured string, input []byte) error {
	if configured != "true" {
		return errors.New("legacy oidc.env requires an explicitly configured siteSecrets.appEnv")
	}
	var app map[string]any
	if err := json.Unmarshal(input, &app); err != nil {
		return errors.New("appEnv must be a string object")
	}
	if app == nil {
		return errors.New("appEnv must be a string object")
	}
	for _, value := range app {
		if _, ok := value.(string); !ok {
			return errors.New("appEnv must be a string object")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	legacy := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, "\r\x00") || strings.HasPrefix(line, "#") {
			return errors.New("legacy oidc.env is malformed")
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || !envKeyPattern.MatchString(parts[0]) || strings.ContainsAny(parts[1], "\r\n\x00") {
			return errors.New("legacy oidc.env is malformed or unsafe")
		}
		if _, exists := legacy[parts[0]]; exists {
			return errors.New("legacy oidc.env contains duplicate keys")
		}
		value := parts[1]
		if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			var decoded string
			if json.Unmarshal([]byte(value), &decoded) != nil {
				return errors.New("legacy oidc.env is malformed")
			}
			value = decoded
		} else if strings.ContainsAny(value, " \t#$`;'\"\\&|<>") {
			return errors.New("legacy oidc.env is malformed or unsafe")
		}
		legacy[parts[0]] = value
	}
	if len(legacy) != len(app) {
		return errors.New("siteSecrets.appEnv does not exactly match legacy oidc.env")
	}
	for key, value := range legacy {
		if app[key] != value {
			return fmt.Errorf("siteSecrets.appEnv does not match legacy oidc.env key %s", key)
		}
	}
	return nil
}

type NeonEndpoint struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	Type     string `json:"type,omitempty"`
	RegionID string `json:"region_id,omitempty"`
}

type neonHTTPStatusError struct{ status int }

func (err *neonHTTPStatusError) Error() string {
	return fmt.Sprintf("Neon API request failed: HTTP %d", err.status)
}

func SelectNeonEndpoint(endpoints []NeonEndpoint, host string) (NeonEndpoint, error) {
	matches := make([]NeonEndpoint, 0, 1)
	for _, endpoint := range endpoints {
		if endpoint.ID != "" && endpoint.Host == host {
			matches = append(matches, endpoint)
		}
	}
	if len(matches) != 1 {
		return NeonEndpoint{}, errors.New("Neon endpoint host did not identify exactly one endpoint")
	}
	return matches[0], nil
}

func NeonRequest(path, method, body string) ([]byte, int, error) {
	key := os.Getenv("NEON_API_KEY")
	if key == "" {
		return nil, 0, errors.New("NEON_API_KEY is required")
	}
	requestBody := io.Reader(nil)
	if body != "" {
		requestBody = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, neonAPIBaseURL+path, requestBody)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := neonHTTPClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if readErr != nil {
		return nil, response.StatusCode, readErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, &neonHTTPStatusError{status: response.StatusCode}
	}
	return data, response.StatusCode, nil
}

func listNeonProjects() ([]map[string]any, error) {
	projects := make([]map[string]any, 0)
	path := "/projects"
	for {
		data, _, err := NeonRequest(path, http.MethodGet, "")
		if err != nil {
			return nil, err
		}
		var envelope map[string]json.RawMessage
		if err := decodeNeonJSON(data, &envelope); err != nil || envelope == nil {
			return nil, errors.New("Neon project list response is malformed")
		}
		rawProjects, ok := envelope["projects"]
		if !ok {
			return nil, errors.New("Neon project list response omitted projects")
		}
		var page []map[string]any
		if err := json.Unmarshal(rawProjects, &page); err != nil || page == nil {
			return nil, errors.New("Neon project list projects must be an array")
		}
		cursor := ""
		if rawPagination, ok := envelope["pagination"]; ok {
			var pagination map[string]json.RawMessage
			if err := json.Unmarshal(rawPagination, &pagination); err != nil || pagination == nil {
				return nil, errors.New("Neon project list pagination is malformed")
			}
			if rawCursor, ok := pagination["cursor"]; ok {
				if string(rawCursor) == "null" || json.Unmarshal(rawCursor, &cursor) != nil {
					return nil, errors.New("Neon project list pagination cursor must be a string")
				}
			}
		}
		for _, project := range page {
			if project == nil {
				return nil, errors.New("Neon project list entry is malformed")
			}
			if _, ok := project["id"].(string); !ok {
				return nil, errors.New("Neon project list entry omitted required id")
			}
			if _, ok := project["name"].(string); !ok {
				return nil, errors.New("Neon project list entry omitted required name")
			}
			if region, ok := project["region_id"]; ok {
				if _, valid := region.(string); !valid {
					return nil, errors.New("Neon project list entry has invalid region metadata")
				}
			}
			projects = append(projects, project)
		}
		if cursor == "" {
			return projects, nil
		}
		path = "/projects?cursor=" + url.QueryEscape(cursor)
	}
}

func parseManagedProject(data []byte) (map[string]any, error) {
	var envelope map[string]any
	if err := decodeNeonJSON(data, &envelope); err != nil || envelope == nil {
		return nil, errors.New("Neon project state is malformed")
	}
	project, ok := envelope["project"].(map[string]any)
	if !ok {
		project = envelope
	}
	for _, key := range []string{"id", "name", "region_id", "default_endpoint_host"} {
		if value, valid := project[key].(string); !valid || value == "" {
			return nil, fmt.Errorf("Neon project state omitted required %s", key)
		}
	}
	return project, nil
}

func acquireNeonLock(path string) (func(), error) {
	lockPath := path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, writeErr := fmt.Fprintf(lock, "{\"pid\":%d}\n", os.Getpid()); writeErr != nil {
			_ = lock.Close()
			_ = os.Remove(lockPath)
			return nil, writeErr
		}
		if closeErr := lock.Close(); closeErr != nil {
			_ = os.Remove(lockPath)
			return nil, closeErr
		}
		return func() { _ = os.Remove(lockPath) }, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	lockData, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		return nil, fmt.Errorf("Neon project state is locked: %s", lockPath)
	}
	var metadata struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(lockData, &metadata) != nil || metadata.PID <= 0 {
		return nil, fmt.Errorf("Neon project state is locked: %s", lockPath)
	}
	if probeErr := neonProcessProbe(metadata.PID); probeErr == nil || !errors.Is(probeErr, syscall.ESRCH) {
		return nil, fmt.Errorf("Neon project state is locked: %s", lockPath)
	}
	stalePath := fmt.Sprintf("%s.%d.stale", lockPath, time.Now().UnixNano())
	if renameErr := os.Rename(lockPath, stalePath); renameErr != nil {
		return nil, fmt.Errorf("Neon project state is locked: %s", lockPath)
	}
	_ = os.Remove(stalePath)
	return acquireNeonLock(path)
}

func ValidateNeonRegion(projectID, host, region string) error {
	if strings.TrimSpace(region) == "" {
		return errors.New("NEON_REGION is required")
	}
	data, _, err := NeonRequest("/projects/"+url.PathEscape(projectID)+"/endpoints", http.MethodGet, "")
	if err != nil {
		return err
	}
	var response struct {
		Endpoints []NeonEndpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return err
	}
	endpoint, err := SelectNeonEndpoint(response.Endpoints, host)
	if err != nil {
		return err
	}
	if endpoint.RegionID == "" {
		return errors.New("Neon endpoint region_id is missing or ambiguous")
	}
	if endpoint.RegionID != region {
		return fmt.Errorf("Neon endpoint region %s does not match configured region %s", endpoint.RegionID, region)
	}
	return nil
}

func ReconcileNeonEndpoint(projectID, host string, min, max float64, suspend int) error {
	data, _, err := NeonRequest("/projects/"+url.PathEscape(projectID)+"/endpoints", http.MethodGet, "")
	if err != nil {
		return err
	}
	var list struct {
		Endpoints []NeonEndpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	endpoint, err := SelectNeonEndpoint(list.Endpoints, host)
	if err != nil {
		return err
	}
	path := "/projects/" + url.PathEscape(projectID) + "/endpoints/" + url.PathEscape(endpoint.ID)
	settings := map[string]json.Number{"autoscaling_limit_min_cu": json.Number(strconv.FormatFloat(min, 'f', -1, 64)), "autoscaling_limit_max_cu": json.Number(strconv.FormatFloat(max, 'f', -1, 64)), "suspend_timeout_seconds": json.Number(strconv.Itoa(suspend))}
	current, _, err := NeonRequest(path, http.MethodGet, "")
	if err != nil {
		return err
	}
	currentBody, err := decodeNeonEndpoint(current)
	if err != nil {
		return err
	}
	if neonSettingsMatch(currentBody, settings) {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"endpoint": settings})
	for attempt := 0; attempt < 5; attempt++ {
		_, status, requestErr := NeonRequest(path, http.MethodPatch, string(body))
		if requestErr == nil {
			break
		}
		if !transient(status) || attempt == 4 {
			return requestErr
		}
		time.Sleep(2 * time.Second)
	}
	for attempt := 0; attempt < 5; attempt++ {
		data, status, requestErr := NeonRequest(path, http.MethodGet, "")
		if requestErr == nil {
			if result, decodeErr := decodeNeonEndpoint(data); decodeErr == nil && neonSettingsMatch(result, settings) {
				return nil
			}
		} else if !transient(status) {
			return requestErr
		}
		if attempt < 4 {
			time.Sleep(2 * time.Second)
		}
	}
	return errors.New("Neon endpoint settings did not converge")
}

func transient(status int) bool {
	return status == 408 || status == 412 || status == 429 || status >= 500 && status <= 599
}

func neonSettingsMatch(values map[string]any, expected map[string]json.Number) bool {
	for key, want := range expected {
		if !neonSettingMatches(values[key], want) {
			return false
		}
	}
	return true
}

func neonSettingMatches(value any, want json.Number) bool {
	actual, ok := value.(json.Number)
	if !ok {
		return false
	}
	actualValue, actualErr := strconv.ParseFloat(actual.String(), 64)
	wantValue, wantErr := strconv.ParseFloat(want.String(), 64)
	return actualErr == nil && wantErr == nil && !math.IsNaN(actualValue) && !math.IsInf(actualValue, 0) && actualValue == wantValue
}

func decodeNeonEndpoint(data []byte) (map[string]any, error) {
	var response struct {
		Endpoint map[string]any `json:"endpoint"`
	}
	if err := decodeNeonJSON(data, &response); err != nil || response.Endpoint == nil {
		return nil, errors.New("Neon endpoint response is malformed")
	}
	return response.Endpoint, nil
}

func decodeNeonJSON(data []byte, value any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON response contains trailing data")
	}
	return nil
}

func CreateOrFindNeonProject(name, region, stateFile string, output io.Writer) error {
	if name == "" {
		return errors.New("NEON_PROJECT_NAME is required")
	}
	if region == "" {
		return errors.New("NEON_REGION is required")
	}
	if stateFile == "" {
		return errors.New("NEON_PROJECT_STATE_FILE is required")
	}
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o700); err != nil {
		return err
	}
	releaseLock, err := acquireNeonLock(stateFile)
	if err != nil {
		return err
	}
	defer releaseLock()
	var project map[string]any
	if data, readErr := os.ReadFile(stateFile); readErr == nil {
		project, err = parseManagedProject(data)
		if err != nil {
			return fmt.Errorf("Neon project state is malformed: %w", err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if project != nil {
		id, ok := project["id"].(string)
		if !ok || id == "" {
			return errors.New("Neon project state is malformed: missing project id")
		}
		current, requestErr := neonProjectDetail(id)
		if requestErr != nil {
			if !neonNotFound(requestErr) {
				return requestErr
			}
		} else if validProject(current, name, region) {
			endpoint, endpointErr := neonDefaultEndpoint(id)
			if endpointErr != nil && !neonNotFound(endpointErr) {
				return endpointErr
			}
			persistedHost, hostOK := project["default_endpoint_host"].(string)
			if endpointErr == nil && hostOK && endpoint == persistedHost {
				current["default_endpoint_host"] = endpoint
				return persistManagedProject(current, stateFile, output)
			}
		}
		// Only a confirmed missing project or verified metadata mismatch can fall through.
	}
	projects, err := listNeonProjects()
	if err != nil {
		return err
	}
	if selected, found, selectErr := selectManagedProject(projects, name, region); selectErr != nil {
		return selectErr
	} else if found {
		return persistManagedProject(selected, stateFile, output)
	}
	body, _ := json.Marshal(map[string]any{"project": map[string]string{"name": name, "region_id": region}})
	created, status, err := NeonRequest("/projects", http.MethodPost, string(body))
	if err != nil {
		if status != http.StatusConflict {
			return err
		}
		recovered, listErr := listNeonProjects()
		if listErr != nil {
			return listErr
		}
		selected, found, selectErr := selectManagedProject(recovered, name, region)
		if selectErr != nil {
			return selectErr
		}
		if !found {
			return errors.New("Neon project creation conflicted but project was not found")
		}
		return persistManagedProject(selected, stateFile, output)
	}
	var envelope map[string]any
	if err := decodeNeonJSON(created, &envelope); err != nil || envelope == nil {
		return errors.New("Neon project creation response is malformed")
	}
	projectMap, _ := envelope["project"].(map[string]any)
	id, _ := projectMap["id"].(string)
	detail, err := neonProjectDetail(id)
	if err != nil {
		return err
	}
	if !validProject(detail, name, region) {
		return errors.New("Neon project response did not match configured project")
	}
	endpoint, err := neonDefaultEndpoint(id)
	if err != nil {
		return err
	}
	detail["default_endpoint_host"] = endpoint
	return persistManagedProject(detail, stateFile, output)
}

func neonProjectDetail(id string) (map[string]any, error) {
	data, _, err := NeonRequest("/projects/"+url.PathEscape(id), http.MethodGet, "")
	if err != nil {
		return nil, err
	}
	var envelope map[string]any
	if err := decodeNeonJSON(data, &envelope); err != nil || envelope == nil {
		return nil, errors.New("Neon project detail is malformed")
	}
	if project, ok := envelope["project"].(map[string]any); ok {
		if !validNeonProjectMetadata(project) {
			return nil, errors.New("Neon project detail is malformed")
		}
		return project, nil
	}
	if !validNeonProjectMetadata(envelope) {
		return nil, errors.New("Neon project detail is malformed")
	}
	return envelope, nil
}

func validNeonProjectMetadata(project map[string]any) bool {
	for _, key := range []string{"id", "name", "region_id"} {
		value, ok := project[key].(string)
		if !ok || value == "" {
			return false
		}
	}
	return true
}

func neonNotFound(err error) bool {
	var statusErr *neonHTTPStatusError
	return errors.As(err, &statusErr) && statusErr.status == http.StatusNotFound
}

func neonDefaultEndpoint(id string) (string, error) {
	data, _, err := NeonRequest("/projects/"+url.PathEscape(id)+"/endpoints", http.MethodGet, "")
	if err != nil {
		return "", err
	}
	var response struct {
		Endpoints []NeonEndpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return "", err
	}
	readWrite := make([]NeonEndpoint, 0)
	untyped := make([]NeonEndpoint, 0)
	for _, endpoint := range response.Endpoints {
		if endpoint.ID != "" && endpoint.Host != "" {
			if endpoint.Type == "read_write" {
				readWrite = append(readWrite, endpoint)
			}
			if endpoint.Type == "" {
				untyped = append(untyped, endpoint)
			}
		}
	}
	if len(readWrite) == 1 && len(untyped) == 0 {
		return readWrite[0].Host, nil
	}
	if len(readWrite) != 0 || len(untyped) != 1 {
		return "", errors.New("Neon project response did not identify exactly one default endpoint")
	}
	return untyped[0].Host, nil
}

func selectManagedProject(projects []map[string]any, name, region string) (map[string]any, bool, error) {
	matches := make([]map[string]any, 0, 1)
	for _, project := range projects {
		if project["name"] == name {
			matches = append(matches, project)
		}
	}
	if len(matches) > 1 {
		return nil, false, errors.New("Neon project name is ambiguous; refusing to select one project")
	}
	if len(matches) == 0 {
		return nil, false, nil
	}
	id, _ := matches[0]["id"].(string)
	detail, err := neonProjectDetail(id)
	if err != nil {
		return nil, false, err
	}
	if !validProject(detail, name, region) {
		return nil, false, errors.New("Neon project region does not match configured region")
	}
	endpoint, err := neonDefaultEndpoint(id)
	if err != nil {
		return nil, false, err
	}
	detail["default_endpoint_host"] = endpoint
	return detail, true, nil
}

func persistManagedProject(project map[string]any, stateFile string, output io.Writer) error {
	managed := make(map[string]string, 4)
	for _, key := range []string{"id", "name", "region_id", "default_endpoint_host"} {
		value, ok := project[key].(string)
		if !ok || value == "" {
			return fmt.Errorf("managed Neon project omitted %s", key)
		}
		managed[key] = value
	}
	encoded, err := json.Marshal(managed)
	if err != nil {
		return err
	}
	if err := atomicWrite(stateFile, append(encoded, '\n'), "neon-project"); err != nil {
		return err
	}
	_, err = output.Write(append(encoded, '\n'))
	return err
}
func validProject(project map[string]any, name, region string) bool {
	return project["name"] == name && project["region_id"] == region
}
func FetchNeonConnection(projectID string, output io.Writer) error {
	database, role := os.Getenv("NEON_DATABASE_NAME"), os.Getenv("NEON_ROLE_NAME")
	if database == "" {
		database = "neondb"
	}
	if role == "" {
		role = "neondb_owner"
	}
	query := url.Values{"database_name": []string{database}, "role_name": []string{role}}
	data, _, err := NeonRequest("/projects/"+url.PathEscape(projectID)+"/connection_uri?"+query.Encode(), http.MethodGet, "")
	if err != nil {
		return err
	}
	var response struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.URI == "" {
		return errors.New("Neon connection URI response omitted uri")
	}
	_, _ = fmt.Fprintln(output, response.URI)
	return nil
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: sub2api-deploy runtime <operation>")
	}
	command := args[0]
	readJSON := func() (map[string]any, error) {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		var values map[string]any
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, err
		}
		if values == nil {
			return nil, errors.New("runtime dotenv input must be a JSON object")
		}
		return values, nil
	}
	switch command {
	case "dotenv":
		if len(args) < 3 {
			return errors.New("usage: runtime dotenv write|write-app PATH")
		}
		if args[1] != "write" && args[1] != "write-app" {
			return errors.New("usage: runtime dotenv write|write-app PATH")
		}
		values, err := readJSON()
		if err != nil {
			return err
		}
		slot, slotData := "", ""
		for _, arg := range args[3:] {
			if strings.HasPrefix(arg, "--slot=") {
				slot = strings.TrimPrefix(arg, "--slot=")
			}
			if strings.HasPrefix(arg, "--slot-data-dir=") {
				slotData = strings.TrimPrefix(arg, "--slot-data-dir=")
			}
		}
		if args[1] == "write" {
			return WriteRuntimeEnv(args[2], values, slot, slotData, contains(args, "--auto-setup=false"))
		}
		return WriteAppEnv(args[2], values)
	case "read-env":
		if len(args) != 3 {
			return errors.New("usage: runtime read-env PATH KEY")
		}
		value, err := ReadRuntimeEnv(args[1], args[2])
		if err != nil {
			return err
		}
		_, _ = io.WriteString(stdout, value)
		return nil
	case "read-state":
		if len(args) != 3 {
			return errors.New("usage: runtime read-state PATH FIELD")
		}
		value, err := ReadStateField(args[1], args[2])
		if err != nil {
			return err
		}
		_, _ = io.WriteString(stdout, value)
		return nil
	case "json-field":
		if len(args) != 3 {
			return errors.New("usage: runtime json-field PATH|--stdin FIELD")
		}
		var value string
		var err error
		if args[1] == "--stdin" {
			data, readErr := io.ReadAll(stdin)
			if readErr != nil {
				return readErr
			}
			value, err = ReadJSONFieldInput(data, args[2])
		} else {
			value, err = ReadJSONField(args[1], args[2])
		}
		if err != nil {
			return err
		}
		_, _ = io.WriteString(stdout, value)
		return nil
	case "has-modes":
		if len(args) != 2 {
			return errors.New("usage: runtime has-modes PATH")
		}
		value, err := HasPersistedModes(args[1])
		if err != nil {
			return err
		}
		if !value {
			return errors.New("deployment state has no persisted modes")
		}
		return nil
	case "mapping-state":
		if len(args) != 3 {
			return errors.New("usage: runtime mapping-state PATH either|complete")
		}
		return CheckMappingState(args[1], args[2])
	case "edge-env":
		if len(args) != 1 {
			return errors.New("usage: runtime edge-env")
		}
		return EdgeEnv(stdout)
	case "swap-state":
		if len(args) != 2 {
			return errors.New("usage: runtime swap-state PATH")
		}
		value, err := SwapState(args[1])
		if err != nil {
			return err
		}
		_, _ = io.WriteString(stdout, value)
		return nil
	case "transition-state":
		if len(args) != 4 {
			return errors.New("usage: runtime transition-state PATH SLOT IMAGE")
		}
		value, err := TransitionState(args[1], args[2], args[3])
		if err != nil {
			return err
		}
		_, _ = io.WriteString(stdout, value)
		return nil
	case "state":
		if len(args) != 4 || args[1] != "write" {
			return errors.New("usage: runtime state write PATH JSON")
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(args[3]), &value); err != nil {
			return err
		}
		return WriteDeployState(args[2], value)
	case "marker":
		if len(args) != 3 || args[1] != "write" {
			return errors.New("usage: runtime marker write PATH")
		}
		return WriteBootstrapMarker(args[2])
	case "deployment-mode":
		if len(args) != 5 {
			return errors.New("usage: runtime deployment-mode check|adopt PATH POSTGRES_MODE REDIS_MODE")
		}
		if args[1] != "check" && args[1] != "adopt" {
			return errors.New("usage: runtime deployment-mode check|adopt PATH POSTGRES_MODE REDIS_MODE")
		}
		state, err := ReadDeployState(args[2])
		if errors.Is(err, os.ErrNotExist) && args[1] == "check" {
			return nil
		}
		if err != nil {
			return err
		}
		if args[1] == "check" {
			return AssertDeploymentModes(state, args[3], args[4], args[2])
		}
		return AdoptDeploymentModes(args[2], args[3], args[4])
	case "host-preflight":
		if len(args) < 4 || len(args) > 6 || args[1] != "check" {
			return errors.New("usage: runtime host-preflight check SITE_IDS HOST_STATE_PATH [PENDING] [EXPECTED]")
		}
		expected := ""
		pending := "false"
		if len(args) > 4 {
			pending = args[4]
		}
		if len(args) > 5 {
			expected = args[5]
		}
		return CheckHostPreflight(args[2], args[3], pending, expected)
	case "host-state":
		if len(args) < 2 {
			return errors.New("usage: runtime host-state write|write-legacy PATH SITE_IDS [pending|complete]")
		}
		if args[1] == "write" {
			if len(args) != 4 {
				return errors.New("usage: runtime host-state write PATH SITE_IDS")
			}
			return WriteHostState(args[2], args[3], "")
		}
		if args[1] == "write-legacy" {
			if len(args) != 5 {
				return errors.New("usage: runtime host-state write-legacy PATH code2 pending|complete")
			}
			return WriteHostState(args[2], args[3], args[4])
		}
		return errors.New("usage: runtime host-state write|write-legacy PATH SITE_IDS [pending|complete]")
	case "preflight":
		if len(args) != 6 || args[1] != "check" {
			return errors.New("usage: runtime preflight check STATE MARKER POSTGRES_MODE REDIS_MODE")
		}
		return ValidateDeploymentPreflight(args[2], args[3], args[4], args[5])
	case "route":
		if len(args) != 8 || args[1] != "write" {
			return errors.New("usage: runtime route write TEMPLATE DESTINATION SITE_ID DOMAIN SLOT ALIAS")
		}
		return WriteSiteRoute(args[2], args[3], args[4], args[5], args[6], args[7])
	case "edge":
		if len(args) != 8 || args[1] != "write" {
			return errors.New("usage: runtime edge write ROOT STATIC_TEMPLATE SING_TEMPLATE EMAIL SERVER TARGET")
		}
		return WriteEdgeConfig(args[2], args[3], args[4], args[5], args[6], args[7])
	case "legacy-env":
		if len(args) != 3 {
			return errors.New("usage: runtime legacy-env PATH CONFIGURED")
		}
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		return VerifyLegacyAppEnv(args[1], args[2], data)
	case "neon-region":
		if len(args) != 1 {
			return errors.New("usage: runtime neon-region")
		}
		if err := ValidateNeonRegion(os.Getenv("NEON_PROJECT_ID"), os.Getenv("NEON_ENDPOINT_HOST"), os.Getenv("NEON_REGION")); err != nil {
			return err
		}
		_, _ = io.WriteString(stdout, "Neon project region validated\n")
		return nil
	case "neon-endpoint":
		if len(args) != 1 {
			return errors.New("usage: runtime neon-endpoint")
		}
		min, err := strconv.ParseFloat(os.Getenv("NEON_AUTOSCALING_MIN_CU"), 64)
		if err != nil {
			return errors.New("Neon endpoint settings are invalid")
		}
		max, err := strconv.ParseFloat(os.Getenv("NEON_AUTOSCALING_MAX_CU"), 64)
		if err != nil {
			return errors.New("Neon endpoint settings are invalid")
		}
		suspend, err := strconv.Atoi(os.Getenv("NEON_SUSPEND_TIMEOUT_SECONDS"))
		if err != nil {
			return errors.New("Neon endpoint settings are invalid")
		}
		if math.IsNaN(min) || math.IsInf(min, 0) || math.IsNaN(max) || math.IsInf(max, 0) {
			return errors.New("Neon endpoint settings are invalid")
		}
		if err := ReconcileNeonEndpoint(os.Getenv("NEON_PROJECT_ID"), os.Getenv("NEON_ENDPOINT_HOST"), min, max, suspend); err != nil {
			return err
		}
		_, _ = io.WriteString(stdout, "Neon endpoint settings reconciled\n")
		return nil
	case "neon-project":
		if len(args) != 1 {
			return errors.New("usage: runtime neon-project")
		}
		return CreateOrFindNeonProject(os.Getenv("NEON_PROJECT_NAME"), os.Getenv("NEON_REGION"), os.Getenv("NEON_PROJECT_STATE_FILE"), stdout)
	case "neon-connection":
		if len(args) != 1 {
			return errors.New("usage: runtime neon-connection")
		}
		return FetchNeonConnection(os.Getenv("NEON_PROJECT_ID"), stdout)
	case "help":
		if len(args) != 1 {
			return errors.New("usage: runtime help")
		}
		_, _ = io.WriteString(stdout, "runtime operations: dotenv read-env state marker deployment-mode host-preflight host-state preflight route edge legacy-env neon-region neon-endpoint neon-project neon-connection\n")
		return nil
	}
	return fmt.Errorf("unknown runtime operation %q", command)
}
