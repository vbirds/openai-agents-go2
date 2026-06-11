package review

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/MitulShah1/openai-agents-go/models"
)

// Defaults applied by New when not overridden by options.
const (
	DefaultModel            = openai.ChatModelGPT4o
	DefaultTimeout          = 5 * time.Minute
	DefaultMaxOutputRetries = 2
	// DefaultMaxInputBytes bounds the rendered prompt (diff + files);
	// roughly 100k tokens of input.
	DefaultMaxInputBytes = 400 * 1024
	// DefaultExplorationTurns is the agent loop budget when a workspace is
	// attached; without one a review is single-shot.
	DefaultExplorationTurns = 16
)

type config struct {
	apiKey           string
	baseURL          string
	client           *openai.Client
	provider         models.ModelProvider
	model            string
	temperature      *float64
	maxOutputTokens  *int
	timeout          time.Duration
	maxTurns         int
	maxOutputRetries int
	maxInputBytes    int
	instructions     string
	extraInstr       string
	logger           *slog.Logger
	deepReview       bool
	specialists      []Specialist
	triageModel      string
}

// Option configures a Reviewer.
type Option func(*config) error

// WithAPIKey sets the OpenAI API key used to build the default client.
// Ignored when WithClient or WithModelProvider is used.
func WithAPIKey(key string) Option {
	return func(c *config) error {
		c.apiKey = key
		return nil
	}
}

// WithBaseURL points the default client at an OpenAI-compatible endpoint
// (e.g. a proxy, gateway, or alternative provider).
func WithBaseURL(url string) Option {
	return func(c *config) error {
		c.baseURL = url
		return nil
	}
}

// WithClient supplies a pre-configured OpenAI client, taking precedence over
// WithAPIKey/WithBaseURL.
func WithClient(client *openai.Client) Option {
	return func(c *config) error {
		c.client = client
		return nil
	}
}

// WithModelProvider supplies a custom model provider, taking precedence over
// all client options. Use this to plug in non-OpenAI backends.
func WithModelProvider(p models.ModelProvider) Option {
	return func(c *config) error {
		c.provider = p
		return nil
	}
}

// WithModel selects the model (default: gpt-4o).
func WithModel(model string) Option {
	return func(c *config) error {
		if model == "" {
			return fmt.Errorf("review: model must not be empty")
		}
		c.model = model
		return nil
	}
}

// WithTemperature sets the sampling temperature. Reviews benefit from low
// temperatures; when unset the model default is used.
func WithTemperature(t float64) Option {
	return func(c *config) error {
		if t < 0 || t > 2 {
			return fmt.Errorf("review: temperature must be in [0, 2], got %v", t)
		}
		c.temperature = &t
		return nil
	}
}

// WithMaxOutputTokens caps the size of the model response.
func WithMaxOutputTokens(n int) Option {
	return func(c *config) error {
		if n <= 0 {
			return fmt.Errorf("review: max output tokens must be positive, got %d", n)
		}
		c.maxOutputTokens = &n
		return nil
	}
}

// WithTimeout bounds the total wall-clock time of a Review call, including
// validation retries (default: 5m). Zero disables the timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *config) error {
		if d < 0 {
			return fmt.Errorf("review: timeout must not be negative")
		}
		c.timeout = d
		return nil
	}
}

// WithMaxTurns caps the agent loop iterations per attempt when exploring a
// workspace (default: 16). It has no effect on single-shot reviews without
// a WorkspaceRoot.
func WithMaxTurns(n int) Option {
	return func(c *config) error {
		if n <= 0 {
			return fmt.Errorf("review: max turns must be positive, got %d", n)
		}
		c.maxTurns = n
		return nil
	}
}

// WithMaxOutputRetries sets how many corrective retries are attempted when
// the model output does not validate against the schema (default: 2).
func WithMaxOutputRetries(n int) Option {
	return func(c *config) error {
		if n < 0 {
			return fmt.Errorf("review: max output retries must not be negative")
		}
		c.maxOutputRetries = n
		return nil
	}
}

// WithMaxInputBytes bounds the rendered prompt size; larger inputs are
// truncated with a warning (default: 400 KiB).
func WithMaxInputBytes(n int) Option {
	return func(c *config) error {
		if n < 1024 {
			return fmt.Errorf("review: max input bytes must be at least 1024, got %d", n)
		}
		c.maxInputBytes = n
		return nil
	}
}

// WithInstructions replaces the built-in system prompt entirely.
func WithInstructions(instructions string) Option {
	return func(c *config) error {
		c.instructions = instructions
		return nil
	}
}

// WithExtraInstructions appends to the system prompt without replacing it.
// Use this for org-wide review conventions that apply to every request.
func WithExtraInstructions(extra string) Option {
	return func(c *config) error {
		c.extraInstr = extra
		return nil
	}
}

// WithDeepReview toggles the multi-agent pipeline: triage routes the change
// to specialist reviewers that run in parallel, and an adjudicator verifies
// their findings against the code before synthesizing the final result.
// Costs more tokens and latency; best for thorough pre-merge review.
func WithDeepReview(enabled bool) Option {
	return func(c *config) error {
		c.deepReview = enabled
		return nil
	}
}

// WithSpecialists replaces the entire specialist roster (built-ins
// included) used by deep-review mode.
func WithSpecialists(specs ...Specialist) Option {
	return func(c *config) error {
		for i := range specs {
			if err := specs[i].Validate(); err != nil {
				return err
			}
		}
		c.specialists = append([]Specialist(nil), specs...)
		return nil
	}
}

// AddSpecialist appends a specialist to the roster, replacing any existing
// specialist with the same name.
func AddSpecialist(spec Specialist) Option {
	return func(c *config) error {
		if err := spec.Validate(); err != nil {
			return err
		}
		for i := range c.specialists {
			if c.specialists[i].Name == spec.Name {
				c.specialists[i] = spec
				return nil
			}
		}
		c.specialists = append(c.specialists, spec)
		return nil
	}
}

// DisableSpecialists removes specialists from the roster by name.
func DisableSpecialists(names ...string) Option {
	return func(c *config) error {
		disabled := make(map[string]bool, len(names))
		for _, n := range names {
			disabled[n] = true
		}
		kept := c.specialists[:0]
		for _, s := range c.specialists {
			if !disabled[s.Name] {
				kept = append(kept, s)
			}
		}
		c.specialists = kept
		return nil
	}
}

// WithTriageModel selects a (typically cheaper) model for the triage stage
// of deep-review mode. Defaults to the main model.
func WithTriageModel(model string) Option {
	return func(c *config) error {
		if model == "" {
			return fmt.Errorf("review: triage model must not be empty")
		}
		c.triageModel = model
		return nil
	}
}

// WithConfigFile loads a .codereview.yaml configuration file and merges it
// into the specialist roster. Options are applied in order, so place this
// after WithSpecialists when combining them.
func WithConfigFile(path string) Option {
	return func(c *config) error {
		fc, err := LoadFileConfig(path)
		if err != nil {
			return err
		}
		merged, err := fc.apply(c.specialists)
		if err != nil {
			return err
		}
		c.specialists = merged
		return nil
	}
}

// WithLogger enables structured logging of run progress and warnings.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) error {
		c.logger = l
		return nil
	}
}

// buildProvider resolves the model provider from the configured options.
func (c *config) buildProvider() (models.ModelProvider, error) {
	if c.provider != nil {
		return c.provider, nil
	}
	if c.client != nil {
		return models.NewOpenAIProvider(c.client), nil
	}
	var opts []option.RequestOption
	if c.apiKey != "" {
		opts = append(opts, option.WithAPIKey(c.apiKey))
	}
	if c.baseURL != "" {
		opts = append(opts, option.WithBaseURL(c.baseURL))
	}
	client := openai.NewClient(opts...)
	return models.NewOpenAIProvider(&client), nil
}
