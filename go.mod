module github.com/openclaw-local/cpa-xai-sentry

go 1.24.0

require (
	github.com/router-for-me/CLIProxyAPI/v7 v7.0.0
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/router-for-me/CLIProxyAPI/v7 => ./third_party/CLIProxyAPI-src
