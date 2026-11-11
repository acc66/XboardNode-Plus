package singbox

import (
	"fmt"

	"github.com/sagernet/sing-box/option"
)

// orderOutbounds installs chain/group dependencies first, regardless of the
// order in which the panel lists them. Reverse order is safe for removal.
func orderOutbounds(outbounds []option.Outbound) ([]option.Outbound, error) {
	byTag := make(map[string]option.Outbound, len(outbounds))
	for _, outbound := range outbounds {
		if outbound.Tag == "" {
			return nil, fmt.Errorf("reload outbound: empty tag")
		}
		if _, exists := byTag[outbound.Tag]; exists {
			return nil, fmt.Errorf("reload outbound: duplicate tag %s", outbound.Tag)
		}
		byTag[outbound.Tag] = outbound
	}
	state := make(map[string]int, len(outbounds))
	ordered := make([]option.Outbound, 0, len(outbounds))
	var visit func(string) error
	visit = func(tag string) error {
		if state[tag] == 2 {
			return nil
		}
		if state[tag] == 1 {
			return fmt.Errorf("reload outbound: dependency cycle at %s", tag)
		}
		outbound, exists := byTag[tag]
		// Dependencies may also be provided by the endpoint manager.
		if !exists {
			return nil
		}
		state[tag] = 1

		for _, dependency := range outboundDependencies(outbound) {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[tag] = 2
		ordered = append(ordered, outbound)
		return nil
	}
	for _, outbound := range outbounds {
		if err := visit(outbound.Tag); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func outboundDependencies(outbound option.Outbound) []string {
	var dependencies []string
	if options, ok := outbound.Options.(option.DialerOptionsWrapper); ok {
		if detour := options.TakeDialerOptions().Detour; detour != "" {
			dependencies = append(dependencies, detour)
		}
	}
	switch options := outbound.Options.(type) {
	case *option.SelectorOutboundOptions:
		dependencies = append(dependencies, options.Outbounds...)
	case *option.URLTestOutboundOptions:
		dependencies = append(dependencies, options.Outbounds...)
	}
	return dependencies
}
