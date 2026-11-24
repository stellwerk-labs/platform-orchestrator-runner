package rolloutexec

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"

	"filippo.io/age"
	"github.com/google/uuid"
	steps "github.com/stellwerk-labs/platform-orchestrator-runner/internal/pipelinesteps"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func (e *Executor) Validate(request Request, selected []Step) error {
	if !validRequest(request) || !filepath.IsAbs(e.Directory) || e.Identity == nil || executableDigest(request.Tool) == "unavailable" ||
		request.RunnerUUID != "" && len(e.catalogueKey) != sha256.Size {
		return errors.New("invalid local rollout")
	}
	if err := e.validateNativeBackend(request); err != nil {
		return err
	}
	return e.validateSteps(selected)
}

// PrepareNativeCatalogue derives an application key from the retained private
// identity. Native callers must check this before exporting or opening storage.
func (e *Executor) PrepareNativeCatalogue(identity *age.X25519Identity) error {
	e.catalogueKey = nil
	if identity == nil {
		return errors.New("native catalogue identity unavailable")
	}
	parsed, err := age.ParseX25519Identity(identity.String())
	if err != nil {
		return errors.New("native catalogue identity unavailable")
	}
	key, err := hkdf.Key(sha256.New, []byte(parsed.String()), nil, "stellwerk.runner.native-catalogue/hmac-sha256/v1", sha256.Size)
	if err != nil {
		return errors.New("native catalogue identity unavailable")
	}
	e.catalogueKey = key
	return nil
}

func (e *Executor) validateSteps(selected []Step) error {
	tools := make(map[string]Tool, len(e.Tools))
	for _, tool := range e.Tools {
		if _, exists := tools[tool.ID]; exists || !identifier.MatchString(tool.ID) {
			return errors.New("invalid local task catalog")
		}
		tools[tool.ID] = tool
	}
	seen := make(map[string]bool, len(selected))
	for _, step := range selected {
		tool, exists := tools[step.ToolID]
		if seen[step.ID] || !identifier.MatchString(step.ID) || !json.Valid(step.Parameters) || !exists {
			return errors.New("invalid rollout task")
		}
		seen[step.ID] = true
		if e.validateTool(tool) != nil {
			return errors.New("local task unavailable")
		}
	}
	return nil
}

func validRequest(request Request) bool {
	for _, value := range []string{request.ID, request.OrgID, request.EnvironmentID, request.RunnerID, request.Backend} {
		if !identifier.MatchString(value) {
			return false
		}
	}
	return ValidRunnerUUID(request.RunnerUUID) && (request.Tool == "opentofu" || request.Tool == "terraform") && validMaterial(request) && json.Valid(request.Manifest)
}

func (e *Executor) validateTool(tool Tool) error {
	definition := tool.Definition
	if tool.Phase != definition.Phase || tool.Retry != definition.Retry {
		return errors.New("inconsistent local task")
	}
	if tool.Phase != "post_apply" && tool.Retry != "read_only" {
		return errors.New("preparation and checks must be read-only")
	}
	if steps.ValidateDefinition(definition) != nil || steps.ValidateEnvironment(tool.Environment) != nil {
		return errors.New("invalid local task")
	}
	for _, capability := range append(append([]string{}, definition.Capabilities...), tool.RequiredCapabilities...) {
		if !contains(e.Capabilities, capability) {
			return errors.New("local task capability unavailable")
		}
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// CatalogDigest binds a delegated rollout to this site's tools, capabilities and IaC binaries.
func (e *Executor) CatalogDigest() string {
	tools := append([]Tool{}, e.Tools...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].ID < tools[j].ID })
	capabilities := append([]string{}, e.Capabilities...)
	sort.Strings(capabilities)
	data, _ := json.Marshal(struct {
		Tools        []Tool            `json:"tools"`
		Capabilities []string          `json:"capabilities"`
		Binaries     map[string]string `json:"binaries"`
		Cost         *costIdentity     `json:"cost,omitempty"`
	}{tools, capabilities, map[string]string{"opentofu": executableDigest("opentofu"), "terraform": executableDigest("terraform")}, e.costIdentity()})
	if len(e.catalogueKey) != 0 {
		mac := hmac.New(sha256.New, e.catalogueKey)
		_, _ = mac.Write(data)
		return hex.EncodeToString(mac.Sum(nil))
	}
	return bytesDigest(data)
}

func executableDigest(tool string) string {
	program := tool
	if tool == "opentofu" {
		program = "tofu"
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return "unavailable"
	}
	file, err := os.Open(path)
	if err != nil {
		return "unavailable"
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func bytesDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validMaterial(request Request) bool {
	if request.RunnerUUID != "" && request.DeploymentToken != "" {
		return false
	}
	if len(request.Bundle) == 0 {
		return request.Configuration != "" && request.Mode == ""
	}
	if len(request.Bundle) > 512<<10 || request.BundleSHA256 != bytesDigest(request.Bundle) || request.Configuration != "" || request.RunnerUUID == "" && request.DeploymentToken == "" {
		return false
	}
	if request.EncryptedOutputsRecipient != "" {
		if _, err := age.ParseX25519Recipient(request.EncryptedOutputsRecipient); err != nil {
			return false
		}
	}
	return request.Mode == "observation" || request.Mode == "deploy" || request.Mode == "rollback" || request.Mode == "plan_only" || request.Mode == "destroy"
}

// ValidRunnerUUID permits the empty legacy profile or an exact native UUID.
func ValidRunnerUUID(value string) bool {
	if value == "" {
		return true
	}
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}
