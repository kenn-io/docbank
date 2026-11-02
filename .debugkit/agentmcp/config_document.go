package agentmcp

import (
	"bytes"
	"errors"
	"io"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

func planDocument(agent Agent, original []byte, name string, entry map[string]any) ([]byte, bool, error) {
	doc := map[string]any{}
	if len(original) > 0 {
		if agent == AgentCodex {
			if _, err := toml.Decode(string(original), &doc); err != nil {
				return nil, false, err
			}
		} else {
			decoder := yaml.NewDecoder(bytes.NewReader(original))
			if err := decoder.Decode(&doc); errors.Is(err, io.EOF) {
				doc = map[string]any{}
			} else if err != nil {
				return nil, false, err
			}
			var extra yaml.Node
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				return nil, false, errors.New("MCP config must contain one YAML document")
			}
		}
		if doc == nil {
			return nil, false, errors.New("MCP config must be a mapping")
		}
	}
	servers := map[string]any{}
	if value, ok := doc["mcp_servers"]; ok {
		var valid bool
		servers, valid = value.(map[string]any)
		if !valid {
			return nil, false, errors.New("mcp_servers must be a mapping")
		}
	}
	if previous, ok := servers[name]; ok {
		before, err := encodeDocument(agent, previous)
		if err != nil {
			return nil, false, err
		}
		after, err := encodeDocument(agent, entry)
		if err != nil {
			return nil, false, err
		}
		if bytes.Equal(before, after) {
			return original, false, nil
		}
	}
	var data []byte
	var err error
	if agent == AgentHermes {
		data, err = editYAML(original, name, entry, servers)
	} else {
		data, err = editTOML(original, name, entry)
	}
	return data, true, err
}

func encodeDocument(agent Agent, value any) ([]byte, error) {
	if agent == AgentHermes {
		return yaml.Marshal(value)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
