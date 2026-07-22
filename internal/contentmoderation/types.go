package contentmoderation

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	ModeOff      = "off"
	ModePreBlock = "pre_block"

	KeywordModeAPIOnly       = "api_only"
	KeywordModeKeywordOnly   = "keyword_only"
	KeywordModeKeywordAndAPI = "keyword_and_api"

	ChannelTypeAuthFile    = "auth_file"
	ChannelTypeProviderKey = "provider_key"
	ChannelTypeProvider    = "provider"

	DefaultBaseURL      = "https://api.openai.com"
	DefaultModel        = "omni-moderation-latest"
	DefaultTimeoutMS    = 3000
	DefaultBlockStatus  = 403
	DefaultBlockMessage = "Your request was blocked by the content moderation policy."
)

var (
	ErrUnavailable     = errors.New("content moderation store unavailable")
	ErrNotFound        = errors.New("content moderation profile not found")
	ErrNameConflict    = errors.New("content moderation profile name already exists")
	ErrVersionConflict = errors.New("content moderation profile version conflict")
	ErrProfileBound    = errors.New("content moderation profile has channel bindings")
	ErrBindingConflict = errors.New("content moderation channel is already bound")
	ErrInvalidChannel  = errors.New("invalid content moderation channel")
)

type Profile struct {
	TenantID        string             `json:"-"`
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	Mode            string             `json:"mode"`
	BaseURL         string             `json:"base_url"`
	Model           string             `json:"model"`
	APIKeySecret    string             `json:"-"`
	TimeoutMS       int                `json:"timeout_ms"`
	KeywordMode     string             `json:"keyword_mode"`
	BlockedKeywords []string           `json:"blocked_keywords"`
	Thresholds      map[string]float64 `json:"thresholds"`
	BlockHTTPStatus int                `json:"block_http_status"`
	BlockMessage    string             `json:"block_message"`
	Version         int64              `json:"version"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

type CreateProfileInput struct {
	Name            string             `json:"name"`
	Mode            string             `json:"mode"`
	BaseURL         string             `json:"base_url"`
	Model           string             `json:"model"`
	APIKey          string             `json:"api_key"`
	TimeoutMS       int                `json:"timeout_ms"`
	KeywordMode     string             `json:"keyword_mode"`
	BlockedKeywords []string           `json:"blocked_keywords"`
	Thresholds      map[string]float64 `json:"thresholds"`
	BlockHTTPStatus int                `json:"block_http_status"`
	BlockMessage    string             `json:"block_message"`
}

type PatchProfileInput struct {
	Name            *string             `json:"name"`
	Mode            *string             `json:"mode"`
	BaseURL         *string             `json:"base_url"`
	Model           *string             `json:"model"`
	APIKey          *string             `json:"api_key"`
	ClearAPIKey     bool                `json:"clear_api_key"`
	TimeoutMS       *int                `json:"timeout_ms"`
	KeywordMode     *string             `json:"keyword_mode"`
	BlockedKeywords *[]string           `json:"blocked_keywords"`
	Thresholds      *map[string]float64 `json:"thresholds"`
	BlockHTTPStatus *int                `json:"block_http_status"`
	BlockMessage    *string             `json:"block_message"`
	Version         int64               `json:"version"`
}

type Binding struct {
	TenantID    string    `json:"-"`
	ChannelType string    `json:"channel_type"`
	ChannelID   string    `json:"channel_id"`
	ProfileID   string    `json:"profile_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type BindingOperation struct {
	ChannelType string  `json:"channel_type"`
	ChannelID   string  `json:"channel_id"`
	ProfileID   *string `json:"profile_id"`
}

type BindingConflictError struct {
	ChannelType       string
	ChannelID         string
	ExistingProfileID string
}

func (e *BindingConflictError) Error() string {
	return fmt.Sprintf("%s %s is already bound to profile %s", e.ChannelType, e.ChannelID, e.ExistingProfileID)
}

func (e *BindingConflictError) Unwrap() error { return ErrBindingConflict }

type ProfileBoundError struct {
	Count int
}

func (e *ProfileBoundError) Error() string {
	return fmt.Sprintf("profile has %d channel binding(s)", e.Count)
}

func (e *ProfileBoundError) Unwrap() error { return ErrProfileBound }

func DefaultThresholds() map[string]float64 {
	return map[string]float64{
		"harassment":             0.98,
		"harassment/threatening": 0.90,
		"hate":                   0.65,
		"hate/threatening":       0.65,
		"illicit":                0.95,
		"illicit/violent":        0.95,
		"self-harm":              0.65,
		"self-harm/intent":       0.85,
		"self-harm/instructions": 0.65,
		"sexual":                 0.65,
		"sexual/minors":          0.65,
		"violence":               0.95,
		"violence/graphic":       0.95,
	}
}

func NewProfile(tenantID, id string, input CreateProfileInput, now time.Time) (Profile, error) {
	profile := Profile{
		TenantID:        strings.TrimSpace(tenantID),
		ID:              strings.TrimSpace(id),
		Name:            input.Name,
		Mode:            input.Mode,
		BaseURL:         input.BaseURL,
		Model:           input.Model,
		APIKeySecret:    input.APIKey,
		TimeoutMS:       input.TimeoutMS,
		KeywordMode:     input.KeywordMode,
		BlockedKeywords: input.BlockedKeywords,
		Thresholds:      input.Thresholds,
		BlockHTTPStatus: input.BlockHTTPStatus,
		BlockMessage:    input.BlockMessage,
		Version:         1,
		CreatedAt:       now.UTC(),
		UpdatedAt:       now.UTC(),
	}
	applyProfileDefaults(&profile)
	return profile, ValidateProfile(profile)
}

func ApplyProfilePatch(profile Profile, patch PatchProfileInput, now time.Time) (Profile, error) {
	if patch.Version <= 0 || patch.Version != profile.Version {
		return Profile{}, ErrVersionConflict
	}
	if patch.Name != nil {
		profile.Name = *patch.Name
	}
	if patch.Mode != nil {
		profile.Mode = *patch.Mode
	}
	if patch.BaseURL != nil {
		profile.BaseURL = *patch.BaseURL
	}
	if patch.Model != nil {
		profile.Model = *patch.Model
	}
	if patch.APIKey != nil {
		if key := strings.TrimSpace(*patch.APIKey); key != "" {
			profile.APIKeySecret = key
		}
	}
	if patch.ClearAPIKey {
		profile.APIKeySecret = ""
	}
	if patch.TimeoutMS != nil {
		profile.TimeoutMS = *patch.TimeoutMS
	}
	if patch.KeywordMode != nil {
		profile.KeywordMode = *patch.KeywordMode
	}
	if patch.BlockedKeywords != nil {
		profile.BlockedKeywords = *patch.BlockedKeywords
	}
	if patch.Thresholds != nil {
		profile.Thresholds = *patch.Thresholds
	}
	if patch.BlockHTTPStatus != nil {
		profile.BlockHTTPStatus = *patch.BlockHTTPStatus
	}
	if patch.BlockMessage != nil {
		profile.BlockMessage = *patch.BlockMessage
	}
	applyProfileDefaults(&profile)
	profile.Version++
	profile.UpdatedAt = now.UTC()
	return profile, ValidateProfile(profile)
}

func applyProfileDefaults(profile *Profile) {
	profile.Name = strings.TrimSpace(profile.Name)
	profile.Mode = strings.ToLower(strings.TrimSpace(profile.Mode))
	if profile.Mode == "" {
		profile.Mode = ModeOff
	}
	profile.BaseURL = strings.TrimRight(strings.TrimSpace(profile.BaseURL), "/")
	if profile.BaseURL == "" {
		profile.BaseURL = DefaultBaseURL
	}
	profile.Model = strings.TrimSpace(profile.Model)
	if profile.Model == "" {
		profile.Model = DefaultModel
	}
	profile.APIKeySecret = strings.TrimSpace(profile.APIKeySecret)
	if profile.TimeoutMS == 0 {
		profile.TimeoutMS = DefaultTimeoutMS
	}
	profile.KeywordMode = strings.ToLower(strings.TrimSpace(profile.KeywordMode))
	if profile.KeywordMode == "" {
		profile.KeywordMode = KeywordModeAPIOnly
	}
	profile.BlockedKeywords = normalizeKeywords(profile.BlockedKeywords)
	profile.Thresholds = mergeThresholds(profile.Thresholds)
	if profile.BlockHTTPStatus == 0 {
		profile.BlockHTTPStatus = DefaultBlockStatus
	}
	profile.BlockMessage = strings.TrimSpace(profile.BlockMessage)
	if profile.BlockMessage == "" {
		profile.BlockMessage = DefaultBlockMessage
	}
}

func ValidateProfile(profile Profile) error {
	if profile.TenantID == "" || profile.ID == "" {
		return errors.New("tenant and profile id are required")
	}
	if profile.Name == "" {
		return errors.New("name is required")
	}
	if profile.Mode != ModeOff && profile.Mode != ModePreBlock {
		return errors.New("mode must be off or pre_block")
	}
	switch profile.KeywordMode {
	case KeywordModeAPIOnly, KeywordModeKeywordOnly, KeywordModeKeywordAndAPI:
	default:
		return errors.New("keyword_mode must be api_only, keyword_only, or keyword_and_api")
	}
	if profile.TimeoutMS <= 0 || profile.TimeoutMS > 30000 {
		return errors.New("timeout_ms must be between 1 and 30000")
	}
	if profile.BlockHTTPStatus < 400 || profile.BlockHTTPStatus > 599 {
		return errors.New("block_http_status must be between 400 and 599")
	}
	if profile.Mode == ModePreBlock && profile.KeywordMode != KeywordModeKeywordOnly && profile.APIKeySecret == "" {
		return errors.New("api_key is required for API moderation in pre_block mode")
	}
	for category, threshold := range profile.Thresholds {
		if strings.TrimSpace(category) == "" || threshold < 0 || threshold > 1 {
			return fmt.Errorf("invalid threshold for category %q", category)
		}
	}
	return nil
}

func normalizeKeywords(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

func mergeThresholds(overrides map[string]float64) map[string]float64 {
	out := DefaultThresholds()
	for category, threshold := range overrides {
		category = strings.TrimSpace(category)
		if category != "" {
			out[category] = threshold
		}
	}
	return out
}

func IsChannelType(value string) bool {
	switch strings.TrimSpace(value) {
	case ChannelTypeAuthFile, ChannelTypeProviderKey, ChannelTypeProvider:
		return true
	default:
		return false
	}
}

func MaskSecret(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	if len(secret) <= 8 {
		return "****"
	}
	return secret[:4] + "****" + secret[len(secret)-4:]
}
