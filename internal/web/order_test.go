package web

import (
	"reflect"
	"testing"
)

func TestOrderVoices(t *testing.T) {
	paths := []string{
		"/m/en_US-amy-medium.onnx",
		"/m/en_US-lessac-medium.onnx",
		"/m/en_GB-alba-medium.onnx",
	}
	tests := []struct {
		name  string
		order []string
		want  []string
	}{
		{
			name:  "no order keeps input order",
			order: nil,
			want:  paths,
		},
		{
			name:  "full order is honored",
			order: []string{"en_GB-alba-medium.onnx", "en_US-lessac-medium.onnx", "en_US-amy-medium.onnx"},
			want: []string{
				"/m/en_GB-alba-medium.onnx",
				"/m/en_US-lessac-medium.onnx",
				"/m/en_US-amy-medium.onnx",
			},
		},
		{
			name:  "partial order, rest appended in input order",
			order: []string{"en_GB-alba-medium.onnx"},
			want: []string{
				"/m/en_GB-alba-medium.onnx",
				"/m/en_US-amy-medium.onnx",
				"/m/en_US-lessac-medium.onnx",
			},
		},
		{
			name:  "unknown and duplicate names are ignored",
			order: []string{"ghost.onnx", "en_US-amy-medium.onnx", "en_US-amy-medium.onnx"},
			want: []string{
				"/m/en_US-amy-medium.onnx",
				"/m/en_US-lessac-medium.onnx",
				"/m/en_GB-alba-medium.onnx",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := orderVoices(paths, tt.order)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("orderVoices() = %v, want %v", got, tt.want)
			}
		})
	}
}
