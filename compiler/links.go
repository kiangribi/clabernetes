// Copyright 2020 Nokia
// Licensed under the BSD 3-Clause License.
// SPDX-License-Identifier: BSD-3-Clause

package compiler

import (
	"fmt"
	"math"
	"strings"
	"time"

	clabernetesapisv1alpha1 "github.com/clabernetes/clabernetes/apis/v1alpha1"
	yamlv3 "gopkg.in/yaml.v3"
)

// Bounds of a link impairment, the same the Link API enforces: the connectivity sidecar programs
// the delay in 32-bit kernel ticks, the kernel caps the jitter at 2^31-1 ns, and the rate in
// kbit/s must convert to bytes/s within 64 bits.
const (
	invalidLinkNetemCode = "invalid-link-netem"

	maxNetemDelay  = 4 * time.Minute
	maxNetemJitter = math.MaxInt32 * time.Nanosecond
	maxNetemRate   = math.MaxUint64 / 1000
)

// linkDefinition represents a link definition in the topology file. It accepts both
// containerlab's brief "node:interface" endpoint syntax and the equivalent structured
// node/interface syntax used by explicit veth links.
type linkDefinition struct {
	linkConfig `yaml:",inline"`

	Type string `yaml:"type,omitempty"`
}

// linkConfig is the link config object the compiler reads.
type linkConfig struct {
	Endpoints linkEndpoints
	Labels    map[string]string `yaml:"labels,omitempty"`
	Vars      map[string]any    `yaml:"vars,omitempty"`
	MTU       int               `yaml:"mtu,omitempty"`
	// Netem is the brief link form of the endpoint impairments, in the order of the endpoints.
	Netem []*netemDefinition `yaml:"netem,omitempty"`
}

// netemDefinition is the link impairment of one endpoint in containerlab's topology vocabulary.
type netemDefinition struct {
	Delay      string  `yaml:"delay,omitempty"`
	Jitter     string  `yaml:"jitter,omitempty"`
	Loss       float64 `yaml:"loss,omitempty"`
	Rate       uint64  `yaml:"rate,omitempty"`
	Corruption float64 `yaml:"corruption,omitempty"`
}

// linkEndpoints stores the canonical brief endpoint form, because that is the complete endpoint
// vocabulary the c9s Link API can represent. The impairment a structured endpoint declares is
// kept beside it, in the same order.
type linkEndpoints struct {
	names []string
	netem []*netemDefinition
}

func (e *linkEndpoints) UnmarshalYAML(value *yamlv3.Node) error {
	if value.Kind != yamlv3.SequenceNode {
		return fmt.Errorf("%w: link endpoints must be a sequence", errParse)
	}

	endpoints := linkEndpoints{
		names: make([]string, 0, len(value.Content)),
		netem: make([]*netemDefinition, 0, len(value.Content)),
	}

	for _, endpointNode := range value.Content {
		switch endpointNode.Kind {
		case yamlv3.ScalarNode:
			endpoint, err := canonicalLinkEndpoint(endpointNode.Value)
			if err != nil {
				return err
			}

			endpoints.names = append(endpoints.names, endpoint)
			endpoints.netem = append(endpoints.netem, nil)
		case yamlv3.MappingNode:
			endpoint := struct {
				Node      string           `yaml:"node"`
				Interface string           `yaml:"interface"`
				Netem     *netemDefinition `yaml:"netem"`
			}{}

			err := endpointNode.Decode(&endpoint)
			if err != nil {
				return fmt.Errorf(
					"%w: decoding structured link endpoint: %w",
					errParse,
					err,
				)
			}

			err = validateStructuredLinkEndpoint(endpointNode)
			if err != nil {
				return err
			}

			canonical, err := canonicalLinkEndpoint(
				fmt.Sprintf("%s:%s", endpoint.Node, endpoint.Interface),
			)
			if err != nil {
				return err
			}

			endpoints.names = append(endpoints.names, canonical)
			endpoints.netem = append(endpoints.netem, endpoint.Netem)
		case yamlv3.DocumentNode, yamlv3.SequenceNode, yamlv3.AliasNode:
			return fmt.Errorf(
				"%w: link endpoint must be a string or node/interface mapping",
				errParse,
			)
		}
	}

	*e = endpoints

	return nil
}

func canonicalLinkEndpoint(value string) (string, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != linkEndpointElementCount {
		return "", fmt.Errorf(
			"%w: link endpoint %q must use node:interface syntax",
			errParse,
			value,
		)
	}

	nodeName := strings.TrimSpace(parts[0])

	interfaceName := strings.TrimSpace(parts[1])
	if nodeName == "" || interfaceName == "" {
		return "", fmt.Errorf(
			"%w: link endpoint %q requires non-empty node and interface",
			errParse,
			value,
		)
	}

	return fmt.Sprintf("%s:%s", nodeName, interfaceName), nil
}

func validateStructuredLinkEndpoint(value *yamlv3.Node) error {
	for index := 0; index+1 < len(value.Content); index += 2 {
		key := value.Content[index]
		field := value.Content[index+1]

		if key.Value != "node" && key.Value != "interface" {
			continue
		}

		if field.Kind != yamlv3.ScalarNode || field.Tag != "!!str" {
			return fmt.Errorf(
				"%w: structured link endpoint field %q must be a string",
				errParse,
				key.Value,
			)
		}
	}

	return nil
}

// compileContainerlabLinks converts the containerlab links section into compiled wires.
func compileContainerlabLinks( //nolint:gocyclo
	fileTopology *fileTopology,
	diagnostics *compileDiagnostics,
) ([]CompiledLink, error) {
	links := make([]CompiledLink, 0, len(fileTopology.Links))

	for linkIndex, link := range fileTopology.Links {
		linkPath := fmt.Sprintf("topology.links[%d]", linkIndex)
		if len(link.Labels) != 0 {
			diagnostics.add(Diagnostic{
				Code:    "unsupported-link-labels",
				Path:    linkPath + ".labels",
				Message: "link labels are not preserved by the c9s Link API",
			})
		}

		if len(link.Vars) != 0 {
			diagnostics.add(Diagnostic{
				Code:    "unsupported-link-vars",
				Path:    linkPath + ".vars",
				Message: "link vars are not preserved by the c9s Link API",
			})
		}

		switch link.Type {
		case "", "brief", "veth":
		default:
			diagnostics.add(unsupportedContainerlabLinkTypeDiagnostic(link.Type, linkPath))

			continue
		}

		if len(link.Endpoints.names) != linkEndpointElementCount {
			return nil, fmt.Errorf(
				"%w: endpoint '%q' has wrong syntax, unexpected number of items",
				errParse,
				link.Endpoints.names,
			)
		}

		endpointAParts := strings.Split(link.Endpoints.names[0], ":")
		endpointBParts := strings.Split(link.Endpoints.names[1], ":")

		if len(endpointAParts) != linkEndpointElementCount ||
			len(endpointBParts) != linkEndpointElementCount {
			return nil, fmt.Errorf(
				"%w: endpoint '%q' has wrong syntax, bad node:interface config",
				errParse,
				link.Endpoints.names,
			)
		}

		netem, invalidNetem := compileLinkNetem(
			link,
			linkPath,
			[2]string{endpointAParts[0], endpointBParts[0]},
			diagnostics,
		)

		invalidEndpoint := false

		for endpointIndex, endpointParts := range [][]string{endpointAParts, endpointBParts} {
			nodeName := endpointParts[0]

			if nodeName == LinkHostNodeName {
				continue
			}

			if nodeName == "mgmt-net" || nodeName == "macvlan" {
				diagnostics.add(Diagnostic{
					Code: "unsupported-special-endpoint",
					Path: fmt.Sprintf("%s.endpoints[%d]", linkPath, endpointIndex),
					Message: fmt.Sprintf(
						"special endpoint %q requires host networking that c9s does not provide",
						nodeName,
					),
				})

				invalidEndpoint = true

				continue
			}

			if _, exists := fileTopology.Nodes[nodeName]; !exists {
				diagnostics.add(Diagnostic{
					Code:    "unknown-link-endpoint",
					Path:    fmt.Sprintf("%s.endpoints[%d]", linkPath, endpointIndex),
					Message: fmt.Sprintf("link endpoint references nonexistent node %q", nodeName),
				})

				invalidEndpoint = true
			}
		}

		if endpointAParts[0] == LinkHostNodeName &&
			endpointBParts[0] == LinkHostNodeName {
			diagnostics.add(Diagnostic{
				Code:    "invalid-host-link",
				Path:    linkPath + ".endpoints",
				Message: "a c9s host link must have exactly one Node endpoint",
			})

			invalidEndpoint = true
		}

		if invalidEndpoint || invalidNetem {
			continue
		}

		links = append(links, CompiledLink{
			EndpointA: Endpoint{
				NodeName:      endpointAParts[0],
				InterfaceName: endpointAParts[1],
				Netem:         netem[0],
			},
			EndpointB: Endpoint{
				NodeName:      endpointBParts[0],
				InterfaceName: endpointBParts[1],
				Netem:         netem[1],
			},
			MTU: link.MTU,
		})
	}

	return links, nil
}

// compileLinkNetem resolves the impairment of each endpoint, from the endpoint itself or from
// the brief link's netem list, into the Link API form. It reports a diagnostic and true for an
// impairment the Link API would reject, so the whole topology fails with a precise path rather
// than one rejected Link.
func compileLinkNetem(
	link *linkDefinition,
	linkPath string,
	nodeNames [2]string,
	diagnostics *compileDiagnostics,
) ([2]*clabernetesapisv1alpha1.LinkNetemSpec, bool) {
	resolved := [2]*clabernetesapisv1alpha1.LinkNetemSpec{}

	if len(link.Netem) > linkEndpointElementCount {
		diagnostics.add(Diagnostic{
			Code:    invalidLinkNetemCode,
			Path:    linkPath + ".netem",
			Message: "the number of netem entries exceeds the number of endpoints",
		})

		return resolved, true
	}

	invalid := false

	for side := range resolved {
		definition := link.Endpoints.netem[side]
		path := fmt.Sprintf("%s.endpoints[%d].netem", linkPath, side)

		if side < len(link.Netem) && link.Netem[side] != nil {
			if definition != nil {
				diagnostics.add(Diagnostic{
					Code:    invalidLinkNetemCode,
					Path:    path,
					Message: "the endpoint and the link's netem list both set this impairment",
				})

				invalid = true

				continue
			}

			definition = link.Netem[side]
			path = fmt.Sprintf("%s.netem[%d]", linkPath, side)
		}

		if definition == nil {
			continue
		}

		if nodeNames[side] == LinkHostNodeName {
			diagnostics.add(Diagnostic{
				Code:    invalidLinkNetemCode,
				Path:    path,
				Message: "a host endpoint cannot be impaired, impair the node endpoint instead",
			})

			invalid = true

			continue
		}

		netem, message := compileNetem(definition)
		if message != "" {
			diagnostics.add(Diagnostic{Code: invalidLinkNetemCode, Path: path, Message: message})

			invalid = true

			continue
		}

		resolved[side] = netem
	}

	return resolved, invalid
}

// compileNetem validates one impairment with the bounds the Link API enforces and returns it in
// the API form, or the reason it is invalid. An impairment without any set field is nil.
func compileNetem(
	definition *netemDefinition,
) (*clabernetesapisv1alpha1.LinkNetemSpec, string) {
	durations := map[string]time.Duration{}

	for _, field := range []struct {
		name    string
		value   string
		maximum time.Duration
	}{
		{name: "delay", value: definition.Delay, maximum: maxNetemDelay},
		{name: "jitter", value: definition.Jitter, maximum: maxNetemJitter},
	} {
		if field.value == "" {
			continue
		}

		duration, err := time.ParseDuration(field.value)
		if err != nil || duration < 0 {
			return nil, fmt.Sprintf(
				"netem %s %q is not a duration, e.g. 100ms",
				field.name,
				field.value,
			)
		}

		if duration > field.maximum {
			return nil, fmt.Sprintf("netem %s must be at most %s", field.name, field.maximum)
		}

		durations[field.name] = duration
	}

	switch {
	case durations["jitter"] != 0 && durations["delay"] == 0:
		return nil, "netem jitter cannot be set without setting delay"
	// written this way, NaN is out of range too
	case !(definition.Loss >= 0 && definition.Loss <= 100):
		return nil, "netem loss must be in the range between 0 and 100"
	case !(definition.Corruption >= 0 && definition.Corruption <= 100):
		return nil, "netem corruption must be in the range between 0 and 100"
	case definition.Rate > maxNetemRate:
		return nil, fmt.Sprintf("netem rate must be at most %d kbit/s", uint64(maxNetemRate))
	}

	if *definition == (netemDefinition{}) {
		return nil, ""
	}

	return &clabernetesapisv1alpha1.LinkNetemSpec{
		Delay:      definition.Delay,
		Jitter:     definition.Jitter,
		Loss:       definition.Loss,
		Rate:       int64(definition.Rate),
		Corruption: definition.Corruption,
	}, ""
}

func unsupportedContainerlabLinkTypeDiagnostic(linkType, linkPath string) Diagnostic {
	message := fmt.Sprintf(
		"native link type %q has no c9s topology-link equivalent",
		linkType,
	)

	if linkType == "host" {
		message = fmt.Sprintf(
			"explicit native link type %q requires structured endpoints that the "+
				"c9s topology compiler does not support; use brief endpoints",
			linkType,
		)
	}

	return Diagnostic{
		Code:    "unsupported-link-type",
		Path:    linkPath + ".type",
		Message: message,
	}
}
