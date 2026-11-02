// Package secretref reads a secret named by one typed configuration value.
//
// In TOML a Ref is either the secret itself or a table naming its source:
//
//	api_key = "sk-inline-value"              # the secret itself
//	api_key = { env = "OPENAI_API_KEY" }     # environment variable
//	api_key = { file = "~/.config/app.key" } # private file; ~/ is home
//
// A string is always the literal secret. Every other source is a table field,
// so a new kind of source, such as a secret manager or a credential helper,
// is a new field and never changes what an existing value means. The table
// form { value = "..." } also holds the literal secret; encoders that write
// struct fields instead of calling MarshalTOML produce it.
//
// Ref decodes with any TOML library that supports encoding.TextUnmarshaler
// for strings and tagged struct fields for tables, and with encoding/json.
package secretref

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/safefileio"
)

// Ref holds a secret or names where it comes from. The zero value is unset.
// Set at most one source; Validate reports more than one.
type Ref struct { //nolint:recvcheck // decoders need pointer receivers; values marshal and resolve as copies
	// Value is the secret itself.
	Value string `toml:"value,omitempty" json:"value,omitempty"`
	// Env names an environment variable that holds the secret.
	Env string `toml:"env,omitempty" json:"env,omitempty"`
	// File is a private file that holds the secret. A leading ~/ means the
	// user's home directory.
	File string `toml:"file,omitempty" json:"file,omitempty"`
}

// Literal returns a Ref that holds the secret itself.
func Literal(secret string) Ref { return Ref{Value: secret} }

// Secret is a resolved Ref. Value goes only to the code that uses the
// secret. Source names where it came from ("inline", "env:NAME", or
// "file:PATH") and never contains the value. Reason explains why a source
// produced no value.
type Secret struct {
	Value  string
	Source string
	Reason string
}

// IsZero reports whether the reference is unset.
func (r Ref) IsZero() bool { return r == Ref{} }

// Validate reports a reference that names more than one source.
func (r Ref) Validate() error {
	var sources []string
	if r.Value != "" {
		sources = append(sources, "value")
	}
	if r.Env != "" {
		sources = append(sources, "env")
	}
	if r.File != "" {
		sources = append(sources, "file")
	}
	if len(sources) > 1 {
		return fmt.Errorf("secretref: set one source, not %s", strings.Join(sources, " and "))
	}
	return nil
}

// UnmarshalText decodes the string form: the secret itself.
func (r *Ref) UnmarshalText(text []byte) error {
	*r = Literal(string(text))
	return nil
}

// UnmarshalTOML decodes a string or a table for TOML decoders that call it
// before trying encoding.TextUnmarshaler, which would reject a table.
func (r *Ref) UnmarshalTOML(data any) error {
	switch v := data.(type) {
	case string:
		*r = Literal(v)
		return nil
	case map[string]any:
		var ref Ref
		for key, raw := range v {
			value, ok := raw.(string)
			if !ok {
				return fmt.Errorf("secretref: %s must be a string", key)
			}
			switch key {
			case "value":
				ref.Value = value
			case "env":
				ref.Env = value
			case "file":
				ref.File = value
			default:
				return fmt.Errorf("secretref: unknown secret source %q", key)
			}
		}
		*r = ref
		return r.Validate()
	default:
		return fmt.Errorf("secretref: a secret is a string or a table, got %T", data)
	}
}

// UnmarshalJSON decodes a JSON string or object.
func (r *Ref) UnmarshalJSON(data []byte) error {
	var literal string
	if err := json.Unmarshal(data, &literal); err == nil {
		*r = Literal(literal)
		return nil
	}
	type fields struct {
		Value string `json:"value"`
		Env   string `json:"env"`
		File  string `json:"file"`
	}
	var decoded fields
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("secretref: a secret is a string or an object: %w", err)
	}
	*r = Ref(decoded)
	return r.Validate()
}

// MarshalTOML encodes the reference in the form the decoders read.
func (r Ref) MarshalTOML() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	switch {
	case r.Env != "":
		return []byte("{ env = " + quote(r.Env) + " }"), nil
	case r.File != "":
		return []byte("{ file = " + quote(r.File) + " }"), nil
	default:
		return []byte(quote(r.Value)), nil
	}
}

// Resolve reads the secret. An unset Ref resolves to an empty Secret.
// A configured environment or file source that is missing, empty, or
// unreadable returns an error. A reference naming more than one source
// also returns an error.
//
// A file must be a regular, private file owned by the current user, as
// safefileio verifies; a symlink or FIFO is refused without blocking.
// Trailing newlines are removed.
func (r Ref) Resolve() (Secret, error) {
	if err := r.Validate(); err != nil {
		return Secret{}, err
	}
	switch {
	case r.IsZero():
		return Secret{}, nil
	case r.Env != "":
		name := strings.TrimSpace(r.Env)
		source := "env:" + name
		value := os.Getenv(name)
		if strings.TrimSpace(value) == "" {
			return Secret{}, fmt.Errorf("secretref: env %q is unset or empty", name)
		}
		return Secret{Value: value, Source: source}, nil
	case r.File != "":
		secret := resolveFile(strings.TrimSpace(r.File))
		if secret.Reason != "" {
			return Secret{}, fmt.Errorf("secretref: %s: %s", secret.Source, secret.Reason)
		}
		return secret, nil
	default:
		if strings.TrimSpace(r.Value) == "" {
			return Secret{Source: "inline", Reason: "inline value is empty"}, nil
		}
		return Secret{Value: r.Value, Source: "inline"}, nil
	}
}

func resolveFile(configured string) Secret {
	unavailable := func(reason string) Secret {
		return Secret{Source: "file:" + configured, Reason: reason}
	}
	path := configured
	if configured == "~" || strings.HasPrefix(configured, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return unavailable("file home directory is unavailable")
		}
		path = filepath.Join(home, strings.TrimPrefix(configured, "~"))
	}
	file, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return unavailable("file is missing")
		}
		return unavailable("file must be a regular file owned by the current user")
	}
	defer func() { _ = file.Close() }()
	if err := safefileio.ValidatePrivateCurrentUserFile(file); err != nil {
		return unavailable("file must be private to its owner (mode 0600)")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return unavailable("file is unreadable")
	}
	value := strings.TrimRight(string(data), "\r\n")
	if strings.TrimSpace(value) == "" {
		return unavailable("file is empty")
	}
	return Secret{Value: value, Source: "file:" + configured}
}

// quote returns a TOML basic string.
func quote(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
