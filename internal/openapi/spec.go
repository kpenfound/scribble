package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	"github.com/kpenfound/scribble/internal/api"
	"github.com/kpenfound/scribble/internal/service"
)

const description = `Scribble serves this API over its Unix socket and, when configured, on a
loopback address (listen.web) and on the tailnet (listen.tailnet). There is no
authentication: the socket, the loopback interface and tailnet membership are
the boundaries. On the web and tailnet listeners, requests must carry a Host
naming the listener, and requests other than GET and HEAD that carry a
cross-origin Origin or a cross-site Sec-Fetch-Site are refused.

Every failure returns {"error":{"code","message"}}. The code is one of
validation (400), forbidden (403), not_found (404), conflict (409),
unsupported_media_type (415), unavailable (503) or internal (500).`

const streamDescription = `A text/event-stream of frames "event: status" with a JSON data line holding the
current status. The first event is sent on connect; further events follow each
change and a 15 second heartbeat.`

// parameters describes each path parameter the routes use.
var parameters = map[string]string{
	"id": "Thought ID: the UUID the capture was submitted with, lower case.",
}

// errorCodes lists the codes the error envelope can carry, in status order.
var errorCodes = []api.Code{api.Validation, api.Forbidden, api.NotFound, api.Conflict, api.UnsupportedMedia, api.Internal, api.Unavailable}

var placeholder = regexp.MustCompile(`\{([a-z]+)\}`)

// object is a JSON object whose keys marshal in a fixed order: the order they
// were added in.
type object struct {
	keys   []string
	values map[string]any
}

func newObject() *object { return &object{values: map[string]any{}} }
func (o *object) set(key string, value any) *object {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
	return o
}
func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		value, err := json.Marshal(o.values[k])
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// schemas collects the named component schemas of the document.
type schemas struct {
	named map[string]*object
	order []string
}

func ref(name string) *object { return newObject().set("$ref", "#/components/schemas/"+name) }

// schema returns the JSON Schema for t. Struct types become named components
// and are referenced.
func (s *schemas) schema(t reflect.Type) (*object, error) {
	switch t.Kind() {
	case reflect.String:
		if t == reflect.TypeOf(api.Code("")) {
			enum := make([]string, len(errorCodes))
			for i, c := range errorCodes {
				enum[i] = string(c)
			}
			return newObject().set("type", "string").set("enum", enum), nil
		}
		return newObject().set("type", "string"), nil
	case reflect.Bool:
		return newObject().set("type", "boolean"), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return newObject().set("type", "integer"), nil
	case reflect.Int64:
		return newObject().set("type", "integer").set("format", "int64"), nil
	case reflect.Slice:
		items, err := s.schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return newObject().set("type", "array").set("items", items), nil
	case reflect.Struct:
		name := t.Name()
		if _, ok := s.named[name]; ok {
			return ref(name), nil
		}
		s.named[name] = nil // reserve against recursion
		s.order = append(s.order, name)
		out := newObject().set("type", "object")
		props := newObject()
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag, ok := f.Tag.Lookup("json")
			if !ok || tag == "-" {
				continue
			}
			field, opts, _ := strings.Cut(tag, ",")
			child, err := s.schema(f.Type)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", name, f.Name, err)
			}
			props.set(field, child)
			if !strings.Contains(opts, "omitempty") {
				required = append(required, field)
			}
		}
		out.set("properties", props)
		if len(required) > 0 {
			out.set("required", required)
		}
		out.set("additionalProperties", false)
		s.named[name] = out
		return ref(name), nil
	}
	return nil, fmt.Errorf("unsupported type %s", t)
}

// Spec returns the indented OpenAPI 3.1 description of service.Routes.
func Spec() ([]byte, error) {
	s := &schemas{named: map[string]*object{}}
	paths := newObject()
	errorResponse, err := s.schema(reflect.TypeOf(api.ErrorResponse{}))
	if err != nil {
		return nil, err
	}
	for _, route := range service.Routes {
		op := newObject().set("operationId", operationID(route)).set("summary", route.Summary)
		desc := route.Description
		if route.Stream {
			desc = streamDescription
		}
		if desc != "" {
			op.set("description", desc)
		}
		var params []*object
		for _, m := range placeholder.FindAllStringSubmatch(route.Path, -1) {
			params = append(params, newObject().set("name", m[1]).set("in", "path").set("required", true).
				set("description", parameters[m[1]]).set("schema", newObject().set("type", "string")))
		}
		if params != nil {
			op.set("parameters", params)
		}
		if route.Request != nil {
			body, err := s.schema(reflect.TypeOf(route.Request))
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", route.Method, route.Path, err)
			}
			op.set("requestBody", newObject().set("content", newObject().set("application/json", newObject().set("schema", body))))
		}
		status := route.Status
		if status == 0 {
			status = http.StatusOK
		}
		response, err := s.schema(reflect.TypeOf(route.Response))
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", route.Method, route.Path, err)
		}
		contentType := "application/json"
		if route.Stream {
			contentType = "text/event-stream"
		}
		op.set("responses", newObject().
			set(fmt.Sprint(status), newObject().set("description", http.StatusText(status)).
				set("content", newObject().set(contentType, newObject().set("schema", response)))).
			set("default", newObject().set("description", "The request failed; the error names its code and reason.").
				set("content", newObject().set("application/json", newObject().set("schema", errorResponse)))))
		item, ok := paths.values[route.Path].(*object)
		if !ok {
			item = newObject()
			paths.set(route.Path, item)
		}
		item.set(strings.ToLower(route.Method), op)
	}
	components := newObject()
	for _, name := range s.order {
		components.set(name, s.named[name])
	}
	host := "The listen.web address, such as 127.0.0.1:8090, or the listen.tailnet hostname."
	doc := newObject().
		set("openapi", "3.1.0").
		set("info", newObject().set("title", "Scribble local API").set("version", "1").set("description", description)).
		set("servers", []*object{newObject().set("url", "http://{host}").set("variables",
			newObject().set("host", newObject().set("default", "scribble").set("description", host)))}).
		set("paths", paths).
		set("components", newObject().set("schemas", components))
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// operationID names route after its method and path, such as
// getThoughtsById for GET /v1/thoughts/{id}.
func operationID(route service.Route) string {
	id := strings.ToLower(route.Method)
	for _, segment := range strings.Split(strings.TrimPrefix(route.Path, api.Prefix+"/"), "/") {
		if name, ok := strings.CutPrefix(segment, "{"); ok {
			segment = "by-" + strings.TrimSuffix(name, "}")
		}
		for _, word := range strings.Split(segment, "-") {
			id += strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return id
}
