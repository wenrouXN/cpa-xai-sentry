package pluginapi

import "time"

// Minimal subset for native usage/management plugins.

type Metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []ConfigField
}

type ConfigFieldType string

const (
	ConfigFieldTypeString  ConfigFieldType = "string"
	ConfigFieldTypeNumber  ConfigFieldType = "number"
	ConfigFieldTypeInteger ConfigFieldType = "integer"
	ConfigFieldTypeBoolean ConfigFieldType = "boolean"
	ConfigFieldTypeEnum    ConfigFieldType = "enum"
	ConfigFieldTypeArray   ConfigFieldType = "array"
	ConfigFieldTypeObject  ConfigFieldType = "object"
)

type ConfigField struct {
	Name        string
	Type        ConfigFieldType
	EnumValues  []string
	Description string
}

type UsageRecord struct {
	Provider        string
	ExecutorType    string
	Model           string
	Alias           string
	APIKey          string
	AuthID          string
	AuthIndex       string
	AuthType        string
	Source          string
	ReasoningEffort string
	ServiceTier     string
	RequestedAt     time.Time
	Latency         time.Duration
	TTFT            time.Duration
	Failed          bool
	Failure         UsageFailure
	Detail          UsageDetail
	ResponseHeaders map[string][]string
}

type UsageFailure struct {
	StatusCode int
	Body       string
}

type UsageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}
