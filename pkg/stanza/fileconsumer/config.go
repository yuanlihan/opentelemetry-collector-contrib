// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fileconsumer // import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/fileconsumer"

import (
	"bufio"
	"fmt"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"go.opentelemetry.io/collector/featuregate"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator/helper"
)

const (
	defaultMaxLogSize         = 1024 * 1024
	defaultMaxConcurrentFiles = 1024
	allowFileDeletion         = "filelog.allowFileDeletion"
)

func init() {
	featuregate.GetRegistry().MustRegisterID(
		allowFileDeletion,
		featuregate.StageAlpha,
		featuregate.WithRegisterDescription("When enabled, allows usage of the `delete_after_read` setting."),
		featuregate.WithRegisterReferenceURL("https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/16314"),
	)
}

// NewConfig creates a new input config with default values
func NewConfig() *Config {
	return &Config{
		IncludeFileName:         true,
		IncludeFilePath:         false,
		IncludeFileNameResolved: false,
		IncludeFilePathResolved: false,
		PollInterval:            200 * time.Millisecond,
		Splitter:                helper.NewSplitterConfig(),
		StartAt:                 "end",
		FingerprintSize:         DefaultFingerprintSize,
		MaxLogSize:              defaultMaxLogSize,
		MaxConcurrentFiles:      defaultMaxConcurrentFiles,
	}
}

// Config is the configuration of a file input operator
type Config struct {
	Finder                  `mapstructure:",squash"`
	IncludeFileName         bool                  `mapstructure:"include_file_name,omitempty"`
	IncludeFilePath         bool                  `mapstructure:"include_file_path,omitempty"`
	IncludeFileNameResolved bool                  `mapstructure:"include_file_name_resolved,omitempty"`
	IncludeFilePathResolved bool                  `mapstructure:"include_file_path_resolved,omitempty"`
	PollInterval            time.Duration         `mapstructure:"poll_interval,omitempty"`
	StartAt                 string                `mapstructure:"start_at,omitempty"`
	FingerprintSize         helper.ByteSize       `mapstructure:"fingerprint_size,omitempty"`
	MaxLogSize              helper.ByteSize       `mapstructure:"max_log_size,omitempty"`
	MaxConcurrentFiles      int                   `mapstructure:"max_concurrent_files,omitempty"`
	DeleteAfterRead         bool                  `mapstructure:"delete_after_read,omitempty"`
	Splitter                helper.SplitterConfig `mapstructure:",squash,omitempty"`
}

// Build will build a file input operator from the supplied configuration
func (c Config) Build(logger *zap.SugaredLogger, emit EmitFunc) (*Manager, error) {
	if c.DeleteAfterRead && !featuregate.GetRegistry().IsEnabled(allowFileDeletion) {
		return nil, fmt.Errorf("`delete_after_read` requires feature gate `%s`", allowFileDeletion)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}

	// Ensure that splitter is buildable
	factory := newMultilineSplitterFactory(c.Splitter.EncodingConfig, c.Splitter.Flusher, c.Splitter.Multiline)
	if _, err := factory.Build(int(c.MaxLogSize)); err != nil {
		return nil, err
	}

	return c.buildManager(logger, emit, factory)
}

// BuildWithSplitFunc will build a file input operator with customized splitFunc function
func (c Config) BuildWithSplitFunc(
	logger *zap.SugaredLogger, emit EmitFunc, splitFunc bufio.SplitFunc) (*Manager, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}

	if splitFunc == nil {
		return nil, fmt.Errorf("must provide split function")
	}

	// Ensure that splitter is buildable
	factory := newCustomizeSplitterFactory(c.Splitter.Flusher, splitFunc)
	if _, err := factory.Build(int(c.MaxLogSize)); err != nil {
		return nil, err
	}

	return c.buildManager(logger, emit, factory)
}

func (c Config) buildManager(logger *zap.SugaredLogger, emit EmitFunc, factory splitterFactory) (*Manager, error) {
	if emit == nil {
		return nil, fmt.Errorf("must provide emit function")
	}
	var startAtBeginning bool
	switch c.StartAt {
	case "beginning":
		startAtBeginning = true
	case "end":
		startAtBeginning = false
	default:
		return nil, fmt.Errorf("invalid start_at location '%s'", c.StartAt)
	}
	return &Manager{
		SugaredLogger: logger.With("component", "fileconsumer"),
		cancel:        func() {},
		readerFactory: readerFactory{
			SugaredLogger: logger.With("component", "fileconsumer"),
			readerConfig: &readerConfig{
				fingerprintSize: int(c.FingerprintSize),
				maxLogSize:      int(c.MaxLogSize),
				emit:            emit,
			},
			fromBeginning:   startAtBeginning,
			splitterFactory: factory,
			encodingConfig:  c.Splitter.EncodingConfig,
		},
		finder:          c.Finder,
		roller:          newRoller(),
		pollInterval:    c.PollInterval,
		maxBatchFiles:   c.MaxConcurrentFiles / 2,
		deleteAfterRead: c.DeleteAfterRead,
		knownFiles:      make([]*Reader, 0, 10),
		seenPaths:       make(map[string]struct{}, 100),
	}, nil
}

func (c Config) validate() error {
	if len(c.Include) == 0 {
		return fmt.Errorf("required argument `include` is empty")
	}

	// Ensure includes can be parsed as globs
	for _, include := range c.Include {
		_, err := doublestar.PathMatch(include, "matchstring")
		if err != nil {
			return fmt.Errorf("parse include glob: %w", err)
		}
	}

	// Ensure excludes can be parsed as globs
	for _, exclude := range c.Exclude {
		_, err := doublestar.PathMatch(exclude, "matchstring")
		if err != nil {
			return fmt.Errorf("parse exclude glob: %w", err)
		}
	}

	if c.MaxLogSize <= 0 {
		return fmt.Errorf("`max_log_size` must be positive")
	}

	if c.MaxConcurrentFiles <= 1 {
		return fmt.Errorf("`max_concurrent_files` must be greater than 1")
	}

	if c.FingerprintSize < MinFingerprintSize {
		return fmt.Errorf("`fingerprint_size` must be at least %d bytes", MinFingerprintSize)
	}

	if c.DeleteAfterRead && c.StartAt == "end" {
		return fmt.Errorf("`delete_after_read` cannot be used with `start_at: end`")
	}

	_, err := c.Splitter.EncodingConfig.Build()
	if err != nil {
		return err
	}
	return nil
}
