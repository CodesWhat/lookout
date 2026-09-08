package drydock

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeswhat/portwing/internal/adapter"
)

// openAPIRefPrefix marks a parsed property whose type comes from another
// schema rather than an inline "type:" line.
const openAPIRefPrefix = "$ref:"

// openAPISchema is the slice of an OpenAPI schema these assertions need: the
// schema's own declared type plus, for an object, each property's declared
// type ("string", "object", ...) or "$ref:<SchemaName>". Reading it out of
// api/openapi.yaml is the point of the test — a hand-copied expectation in Go
// would drift with the document instead of catching the drift.
type openAPISchema struct {
	kind       string
	properties map[string]string
}

// loadOpenAPISchemas parses components.schemas out of api/openapi.yaml.
//
// The document is uniformly two-space indented, so the schema names sit at
// indent 4, their keys at 6, property names at 8 and property keys at 10.
// Scanning those fixed depths reads every type declaration the assertions
// need without pulling a YAML parser into the module's dependency set.
// Deeper lines (folded descriptions, examples, enums) never land on a depth
// the scanner reads, so they are ignored rather than misparsed.
func loadOpenAPISchemas(t *testing.T) map[string]*openAPISchema {
	t.Helper()

	path := filepath.Join("..", "..", "..", "api", "openapi.yaml")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()

	schemas := make(map[string]*openAPISchema)
	var current *openAPISchema
	inSchemas := false
	inProperties := false
	property := ""

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		switch {
		case indent == 0:
			inSchemas, current, inProperties, property = false, nil, false, ""
		case indent == 2:
			inSchemas = trimmed == "schemas:"
			current, inProperties, property = nil, false, ""
		case !inSchemas:
			// Inside some other top-level section.
		case indent == 4 && strings.HasSuffix(trimmed, ":"):
			current = &openAPISchema{properties: make(map[string]string)}
			schemas[strings.TrimSuffix(trimmed, ":")] = current
			inProperties, property = false, ""
		case current == nil:
			// A stray line before the first schema name.
		case indent == 6:
			inProperties, property = trimmed == "properties:", ""
			if kind, ok := strings.CutPrefix(trimmed, "type: "); ok {
				current.kind = kind
			}
		case indent == 8 && inProperties && strings.HasSuffix(trimmed, ":"):
			property = strings.TrimSuffix(trimmed, ":")
			current.properties[property] = ""
		case indent == 10 && property != "":
			if kind, ok := strings.CutPrefix(trimmed, "type: "); ok {
				current.properties[property] = kind
			}
			if ref, ok := strings.CutPrefix(trimmed, "$ref: '#/components/schemas/"); ok {
				current.properties[property] = openAPIRefPrefix + strings.TrimSuffix(ref, "'")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(schemas) == 0 {
		t.Fatalf("%s: parsed no schemas", path)
	}
	return schemas
}

// openAPIKind names a decoded JSON value's type in OpenAPI's vocabulary.
func openAPIKind(value any) string {
	switch value.(type) {
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "null"
	}
}

// assertMatchesOpenAPISchema checks that every field the agent serves is
// declared by the named schema and carries the declared JSON type, following
// $refs into nested schemas. A schema that declares no properties (the
// free-form RuntimeDetails) is checked for objectness and left alone.
func assertMatchesOpenAPISchema(
	t *testing.T,
	schemas map[string]*openAPISchema,
	schemaName string,
	value any,
	path string,
) {
	t.Helper()

	schema, ok := schemas[schemaName]
	if !ok {
		t.Fatalf("%s: api/openapi.yaml declares no %s schema", path, schemaName)
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s: served as %s, api/openapi.yaml declares object", path, openAPIKind(value))
	}
	if len(schema.properties) == 0 {
		return
	}

	for name, served := range object {
		declared, known := schema.properties[name]
		if !known {
			t.Errorf("%s.%s is served but api/openapi.yaml's %s schema does not declare it", path, name, schemaName)
			continue
		}
		if ref, isRef := strings.CutPrefix(declared, openAPIRefPrefix); isRef {
			assertMatchesOpenAPISchema(t, schemas, ref, served, path+"."+name)
			continue
		}
		got := openAPIKind(served)
		if declared == "integer" && got == "number" {
			continue
		}
		if got != declared {
			t.Errorf("%s.%s is served as %s, api/openapi.yaml declares %s", path, name, got, declared)
		}
	}
}

// TestContainersResponseMatchesOpenAPISchema pins the JSON that
// GET /api/containers actually serves to the types api/openapi.yaml
// publishes, so the two can't drift apart again.
//
// image.tag and updateKind are why it exists: Drydock's AgentClient stores
// the container verbatim and expects objects there ({value, semver} and
// {kind}), which is what the agent sends and what
// docs/drydock-integration.md documents, while the OpenAPI document declared
// both as plain strings. Every client generated from the published contract
// failed to decode a real response.
func TestContainersResponseMatchesOpenAPISchema(t *testing.T) {
	t.Parallel()

	schemas := loadOpenAPISchemas(t)

	client, _, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})
	if _, err := a.containers.BuildInventory(context.Background()); err != nil {
		t.Fatalf("build inventory: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/containers", nil)
	rec := httptest.NewRecorder()
	a.handleContainers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var served []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&served); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(served) != 1 {
		t.Fatalf("containers = %d, want 1", len(served))
	}
	assertMatchesOpenAPISchema(t, schemas, "Container", served[0], "container")

	// The two fields the contract broke on, asserted by name so a future
	// edit can't quietly turn both the schema and the wire back into
	// strings and still satisfy the type comparison above.
	image, ok := served[0]["image"].(map[string]any)
	if !ok {
		t.Fatalf("image is served as %s, want object", openAPIKind(served[0]["image"]))
	}
	if _, ok := image["tag"].(map[string]any); !ok {
		t.Fatalf("image.tag is served as %s, want the {value, semver} object Drydock consumes", openAPIKind(image["tag"]))
	}
	if _, ok := served[0]["updateKind"].(map[string]any); !ok {
		t.Fatalf("updateKind is served as %s, want the {kind} object Drydock consumes", openAPIKind(served[0]["updateKind"]))
	}
	for _, field := range []struct{ schema, property, want string }{
		{schema: "ContainerImage", property: "tag", want: openAPIRefPrefix + "ContainerImageTag"},
		{schema: "Container", property: "updateKind", want: openAPIRefPrefix + "ContainerUpdateKind"},
	} {
		if got := schemas[field.schema].properties[field.property]; got != field.want {
			t.Errorf("api/openapi.yaml %s.%s = %q, want %q", field.schema, field.property, got, field.want)
		}
	}
}

// TestFullyPopulatedContainerMatchesOpenAPISchema covers the optional fields
// a live inventory omits — displayIcon, health, result, error — which the
// served-response test above never populates.
func TestFullyPopulatedContainerMatchesOpenAPISchema(t *testing.T) {
	t.Parallel()

	schemas := loadOpenAPISchemas(t)

	encoded, err := json.Marshal(toDrydockContainer(adapter.Container{
		ID:          "container-1",
		Name:        "web",
		DisplayName: "Web",
		DisplayIcon: "docker",
		Status:      "running",
		Watcher:     "docker",
		Agent:       "test-agent",
		Image: adapter.ContainerImage{
			ID:           "sha256:1234",
			Registry:     "ghcr.io",
			Name:         "owner/repo",
			Tag:          "1.27.0",
			Digest:       "sha256:abcd",
			Architecture: "arm64",
			OS:           "linux",
			Created:      "2026-01-01T00:00:00Z",
		},
		Result:          &adapter.ContainerResult{Tag: "1.27.1", Digest: "sha256:beef", Created: "2026-01-02T00:00:00Z", Link: "https://example.invalid/repo"},
		Error:           &adapter.ContainerError{Message: "inspect failed", Timestamp: "2026-01-02T00:00:00Z"},
		UpdateAvailable: true,
		UpdateKind:      adapter.UpdateKindUnknown,
		IncludeTags:     "^1\\.",
		ExcludeTags:     "-rc",
		TransformTags:   "^(.*)$ => $1",
		Labels:          map[string]string{"dd.watch": "true"},
		Details: &adapter.RuntimeDetails{
			Health:  "healthy",
			Ports:   []adapter.PortMapping{{Container: 80, Host: 8080, Protocol: "tcp"}},
			Volumes: []adapter.VolumeInfo{{Source: "/srv/data", Destination: "/data"}},
			Env:     []adapter.EnvVar{{Key: "TZ", Value: "UTC"}},
			Started: "2026-01-01T00:00:00Z",
		},
	}))
	if err != nil {
		t.Fatalf("marshal container: %v", err)
	}

	var served map[string]any
	if err := json.Unmarshal(encoded, &served); err != nil {
		t.Fatalf("decode container: %v", err)
	}
	for _, field := range []string{"displayIcon", "health", "result", "error"} {
		if _, ok := served[field]; !ok {
			t.Fatalf("%s missing from the marshalled container; the fixture no longer covers it", field)
		}
	}
	assertMatchesOpenAPISchema(t, schemas, "Container", served, "container")
	containerError, ok := served["error"].(map[string]any)
	if !ok || containerError["timestamp"] != "2026-01-02T00:00:00Z" {
		t.Fatalf("error timestamp not preserved: %v", served["error"])
	}
}
