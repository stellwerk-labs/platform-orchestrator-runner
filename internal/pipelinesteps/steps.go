// Package steps runs trusted catalog tasks through a bounded JSON protocol.
package steps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/processgroup"
)

const ProtocolVersion = 1
const maxMessage = 64 << 10
const maxFile = 64 << 20

type Definition struct {
	ProtocolVersion   int      `json:"protocol_version"`
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	Phase             string   `json:"phase"`
	Executable        string   `json:"executable"`
	ExecutableSHA256  string   `json:"executable_sha256"`
	Interpreter       string   `json:"interpreter,omitempty"`
	InterpreterSHA256 string   `json:"interpreter_sha256,omitempty"`
	Retry             string   `json:"retry"`
	Capabilities      []string `json:"capabilities"`
}
type Input struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Request struct {
	Version    int              `json:"version"`
	TaskID     string           `json:"task_id"`
	EffectID   string           `json:"effect_id"`
	Inputs     map[string]Input `json:"inputs"`
	Parameters json.RawMessage  `json:"parameters"`
	WorkDir    string           `json:"work_dir"`
}
type Result struct {
	TaskID   string           `json:"task_id"`
	EffectID string           `json:"effect_id"`
	Version  int              `json:"version"`
	Decision string           `json:"decision"`
	Category string           `json:"category"`
	Outputs  map[string]Input `json:"outputs"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
var envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

// Execute runs only locally trusted executables. Capabilities are metadata, not
// an isolation boundary. The caller must enforce access and retry policy.
// Errors are deliberately generic: task stderr, input bytes and env never escape.
func Execute(ctx context.Context, def Definition, req Request, env map[string]string) (Result, error) {
	if err := validateDefinition(def); err != nil {
		return Result{}, err
	}
	data, err := encodeRequest(def, req, env)
	if err != nil {
		return Result{}, err
	}
	output, err := runProcess(ctx, def, req.WorkDir, env, data)
	if err != nil {
		return Result{}, err
	}
	return decodeResult(def.Phase, req, output)
}

func validateDefinition(def Definition) error {
	if def.ProtocolVersion != 1 || !namePattern.MatchString(def.Name) || !namePattern.MatchString(def.Version) || !oneOf(def.Phase, "pre_plan", "plan_check", "post_apply") || !oneOf(def.Retry, "read_only", "receipt", "unsafe") {
		return errors.New("invalid task definition")
	}
	if verifyFile(def.Executable, def.ExecutableSHA256, maxFile) != nil {
		return errors.New("invalid executable")
	}
	if def.Interpreter == "" {
		if def.InterpreterSHA256 != "" {
			return errors.New("invalid interpreter")
		}
	} else if verifyFile(def.Interpreter, def.InterpreterSHA256, maxFile) != nil {
		return errors.New("invalid interpreter")
	}
	return nil
}

func encodeRequest(def Definition, req Request, env map[string]string) ([]byte, error) {
	if err := validateRequest(def, req); err != nil {
		return nil, err
	}
	data, err := json.Marshal(req)
	if err != nil || len(data) > maxMessage {
		return nil, errors.New("invalid task request")
	}
	if err := validateEnvironment(env); err != nil {
		return nil, err
	}
	return data, nil
}

func validateRequest(def Definition, req Request) error {
	if req.Version != 1 || !namePattern.MatchString(req.TaskID) || (req.EffectID != "" && !namePattern.MatchString(req.EffectID)) || (def.Retry != "read_only" && req.EffectID == "") || !filepath.IsAbs(req.WorkDir) || !json.Valid(req.Parameters) {
		return errors.New("invalid task request")
	}
	info, err := os.Stat(req.WorkDir)
	if err != nil || !info.IsDir() {
		return errors.New("invalid work directory")
	}
	return validateInputs(req.Inputs)
}

func validateInputs(inputs map[string]Input) error {
	for name, in := range inputs {
		if !namePattern.MatchString(name) || verifyFile(in.Path, in.SHA256, maxFile) != nil {
			return errors.New("invalid input")
		}
	}
	return nil
}

func validateEnvironment(env map[string]string) error {
	for k, v := range env {
		if !envPattern.MatchString(k) || strings.IndexByte(v, 0) >= 0 || len(v) > maxMessage {
			return errors.New("invalid task environment")
		}
	}
	return nil
}

func runProcess(ctx context.Context, def Definition, dir string, env map[string]string, data []byte) ([]byte, error) {
	program := def.Executable
	var args []string
	if def.Interpreter != "" {
		program = def.Interpreter
		args = []string{def.Executable}
	}
	cmd := exec.Command(program, args...) // #nosec G204 -- trusted catalog executables and interpreter were verified by digest.
	cmd.Dir = dir
	cmd.Env = []string{}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = bytes.NewReader(data)
	output := &limitedBuffer{limit: maxMessage}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	err := processgroup.Run(ctx, cmd)
	if err != nil || output.overflow {
		return nil, errors.New("task execution failed")
	}
	return output.Bytes(), nil
}

func decodeResult(phase string, req Request, data []byte) (Result, error) {
	var result Result
	if !uniqueJSON(data) {
		return result, errors.New("invalid task result")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || !validResult(phase, req, result) {
		return Result{}, errors.New("invalid task result")
	}
	for name, out := range result.Outputs {
		if !namePattern.MatchString(name) || !inside(req.WorkDir, out.Path) || verifyFile(out.Path, out.SHA256, maxFile) != nil {
			return Result{}, errors.New("invalid task output")
		}
	}
	return result, nil
}

func validResult(phase string, req Request, result Result) bool {
	return result.Version == 1 && result.TaskID == req.TaskID && result.EffectID == req.EffectID && validDecision(phase, result.Decision, result.Category)
}

func validDecision(phase, decision, category string) bool {
	switch decision {
	case "allow":
		return category == "ok"
	case "deny":
		return phase == "plan_check" && category == "policy"
	case "fault":
		return oneOf(category, "invalid_input", "execution", "dependency", "unknown_outcome")
	default:
		return false
	}
}
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func inside(dir, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	// EvalSymlinks rejects symlinks anywhere in the path, including parent folders.
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return false
	}
	base, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(base, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	// Permit symlinks in the work directory's ancestors, but none below it.
	expected := filepath.Join(base, strings.TrimPrefix(clean, filepath.Clean(dir)+string(filepath.Separator)))
	return resolved == expected
}
func verifyFile(path, expected string, limit int64) error {
	if !filepath.IsAbs(path) || len(expected) != 64 || strings.ToLower(expected) != expected {
		return errors.New("invalid file")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("invalid file")
	}
	f, err := os.Open(path) // #nosec G304 -- absolute regular file verified against the frozen digest.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil || n > limit || hex.EncodeToString(h.Sum(nil)) != expected {
		return errors.New("digest mismatch")
	}
	return nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.overflow = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// uniqueJSON rejects ambiguous duplicate object keys at every nesting level.
func uniqueJSON(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	if !uniqueValue(d) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func uniqueValue(d *json.Decoder) bool {
	token, err := d.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '{':
		return uniqueObject(d)
	case '[':
		return uniqueArray(d)
	default:
		return false
	}
}

func uniqueObject(d *json.Decoder) bool {
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return false
		}
		s, ok := key.(string)
		if !ok || seen[s] {
			return false
		}
		seen[s] = true
		if !uniqueValue(d) {
			return false
		}
	}
	end, err := d.Token()
	return err == nil && end == json.Delim('}')
}

func uniqueArray(d *json.Decoder) bool {
	for d.More() {
		if !uniqueValue(d) {
			return false
		}
	}
	end, err := d.Token()
	return err == nil && end == json.Delim(']')
}
