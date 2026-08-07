package pluginrpc

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// toStruct turns a Go value into the protobuf Struct used by the wire
// protocol. JSON is intentional here: the public contract is JSON-compatible
// data, not a lossy reflection bridge for arbitrary Go values.
func toStruct(value any) (*structpb.Struct, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode external plugin payload: %w", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, fmt.Errorf("normalise external plugin payload: %w", err)
	}
	result, err := structpb.NewStruct(decoded)
	if err != nil {
		return nil, fmt.Errorf("build external plugin protobuf payload: %w", err)
	}
	return result, nil
}

func fromStruct(value *structpb.Struct, target any) error {
	if value == nil {
		return fmt.Errorf("decode external plugin payload: response is nil")
	}
	encoded, err := json.Marshal(value.AsMap())
	if err != nil {
		return fmt.Errorf("encode external plugin protobuf response: %w", err)
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return fmt.Errorf("decode external plugin response: %w", err)
	}
	return nil
}

func structMap(value *structpb.Struct) map[string]any {
	if value == nil {
		return nil
	}
	return value.AsMap()
}
