/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"

	"knative.dev/pkg/changeset"
	"knative.dev/pkg/logging/logkey"
)

const ConfigMapNameEnv = "CONFIG_LOGGING_NAME"

var zapLoggerConfig = "zap-logger-config"

// NewLogger creates a logger with the supplied configuration.
// In addition to the logger, it returns AtomicLevel that can
// be used to change the logging level at runtime.
// If configuration is empty, a fallback configuration is used.
// If configuration cannot be used to instantiate a logger,
// the same fallback configuration is used.
func NewLogger(configJSON string, levelOverride string, opts ...zap.Option) (*zap.SugaredLogger, zap.AtomicLevel) {
	logger, atomicLevel, err := newLoggerFromConfig(configJSON, levelOverride, opts)
	if err == nil {
		return enrichLoggerWithCommitID(logger.Sugar()), atomicLevel
	}

	loggingCfg := zap.NewProductionConfig()
	if len(levelOverride) > 0 {
		if level, err := levelFromString(levelOverride); err == nil {
			loggingCfg.Level = zap.NewAtomicLevelAt(*level)
		}
	}

	logger, err2 := loggingCfg.Build(opts...)
	if err2 != nil {
		panic(err2)
	}
	return enrichLoggerWithCommitID(logger.Named("fallback-logger").Sugar()), loggingCfg.Level
}

func enrichLoggerWithCommitID(logger *zap.SugaredLogger) *zap.SugaredLogger {
	commmitID, err := changeset.Get()
	if err == nil {
		// Enrich logs with GitHub commit ID.
		return logger.With(zap.String(logkey.GitHubCommitID, commmitID))
	}

	logger.Infof("Fetch GitHub commit ID from kodata failed: %v", err)
	return logger
}

// NewLoggerFromConfig creates a logger using the provided Config
func NewLoggerFromConfig(config *Config, name string, opts ...zap.Option) (*zap.SugaredLogger, zap.AtomicLevel) {
	logger, level := NewLogger(config.LoggingConfig, config.LoggingLevel[name].String(), opts...)
	return logger.Named(name), level
}

func newLoggerFromConfig(configJSON string, levelOverride string, opts []zap.Option) (*zap.Logger, zap.AtomicLevel, error) {
	if len(configJSON) == 0 {
		return nil, zap.AtomicLevel{}, errors.New("empty logging configuration")
	}

	var loggingCfg zap.Config
	if err := json.Unmarshal([]byte(configJSON), &loggingCfg); err != nil {
		return nil, zap.AtomicLevel{}, err
	}

	if len(levelOverride) > 0 {
		if level, err := levelFromString(levelOverride); err == nil {
			loggingCfg.Level = zap.NewAtomicLevelAt(*level)
		}
	}

	logger, err := loggingCfg.Build(opts...)
	if err != nil {
		return nil, zap.AtomicLevel{}, err
	}

	logger.Info("Successfully created the logger.", zap.String(logkey.JSONConfig, configJSON))
	logger.Sugar().Infof("Logging level set to %v", loggingCfg.Level)
	return logger, loggingCfg.Level, nil
}

// Config contains the configuration defined in the logging ConfigMap.
// +k8s:deepcopy-gen=true
type Config struct {
	LoggingConfig string
	LoggingLevel  map[string]zapcore.Level
}

const defaultZLC = `{
  "level": "info",
  "development": false,
  "outputPaths": ["stdout"],
  "errorOutputPaths": ["stderr"],
  "encoding": "json",
  "encoderConfig": {
    "timeKey": "ts",
    "levelKey": "level",
    "nameKey": "logger",
    "callerKey": "caller",
    "messageKey": "msg",
    "stacktraceKey": "stacktrace",
    "lineEnding": "",
    "levelEncoder": "",
    "timeEncoder": "iso8601",
    "durationEncoder": "",
    "callerEncoder": ""
  }
}`

// NewConfigFromMap creates a LoggingConfig from the supplied map,
// expecting the given list of components.
func NewConfigFromMap(data map[string]string) (*Config, error) {
	lc := &Config{}
	if zlc, ok := data["zap-logger-config"]; ok {
		lc.LoggingConfig = zlc
	} else {
		lc.LoggingConfig = defaultZLC
	}

	lc.LoggingLevel = make(map[string]zapcore.Level)
	for k, v := range data {
		if component := strings.TrimPrefix(k, "loglevel."); component != k && component != "" {
			if len(v) > 0 {
				level, err := levelFromString(v)
				if err != nil {
					return nil, err
				}
				lc.LoggingLevel[component] = *level
			}
		}
	}
	return lc, nil
}

// NewConfigFromConfigMap creates a LoggingConfig from the supplied ConfigMap,
// expecting the given list of components.
func NewConfigFromConfigMap(configMap *corev1.ConfigMap) (*Config, error) {
	return NewConfigFromMap(configMap.Data)
}

func levelFromString(level string) (*zapcore.Level, error) {
	var zapLevel zapcore.Level
	if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid logging level: %v", level)
	}
	return &zapLevel, nil
}

// UpdateLevelFromConfigMap returns a helper func that can be used to update the logging level
// when a config map is updated
func UpdateLevelFromConfigMap(logger *zap.SugaredLogger, atomicLevel zap.AtomicLevel,
	levelKey string) func(configMap *corev1.ConfigMap) {
	return func(configMap *corev1.ConfigMap) {
		loggingConfig, err := NewConfigFromConfigMap(configMap)
		if err != nil {
			logger.Errorw("Failed to parse the logging configmap. Previous config map will be used.", zap.Error(err))
			return
		}

		level := loggingConfig.LoggingLevel[levelKey]
		if atomicLevel.Level() != level {
			logger.Infof("Updating logging level for %v from %v to %v.", levelKey, atomicLevel.Level(), level)
			atomicLevel.SetLevel(level)
		}
	}
}

// ConfigMapName gets the name of the logging ConfigMap
func ConfigMapName() string {
	cm := os.Getenv(ConfigMapNameEnv)
	if cm == "" {
		return "config-logging"
	}
	return cm
}

// JsonToLoggingConfig converts a json string of a Config.
// Returns a non-nil Config always.
func JsonToLoggingConfig(jsonCfg string) (*Config, error) {
	if jsonCfg == "" {
		return nil, errors.New("json logging string is empty")
	}

	var configMap map[string]string
	if err := json.Unmarshal([]byte(jsonCfg), &configMap); err != nil {
		return nil, err
	}

	cfg, err := NewConfigFromMap(configMap)
	if err != nil {
		// Get the default config from logging package.
		if cfg, err = NewConfigFromMap(nil); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// LoggingConfigToJson converts a Config to a json string.
func LoggingConfigToJson(cfg *Config) (string, error) {
	if cfg == nil || cfg.LoggingConfig == "" {
		return "", nil
	}

	jsonCfg, err := json.Marshal(map[string]string{
		zapLoggerConfig: cfg.LoggingConfig,
	})
	if err != nil {
		return "", err
	}

	return string(jsonCfg), nil
}
