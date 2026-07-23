package engine

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// writeWAVFile writes mono float32 PCM as a 16-bit PCM WAV file.
func writeWAVFile(path string, pcm []float32, sampleRate int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	dataLen := len(pcm) * 2
	var hdr [44]byte
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(36+dataLen))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16) // PCM fmt chunk size
	binary.LittleEndian.PutUint16(hdr[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(hdr[22:24], 1)  // mono
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(sampleRate*2)) // byte rate
	binary.LittleEndian.PutUint16(hdr[32:34], 2)                    // block align
	binary.LittleEndian.PutUint16(hdr[34:36], 16)                   // bits/sample
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], uint32(dataLen))
	if _, err := f.Write(hdr[:]); err != nil {
		return err
	}

	buf := make([]byte, dataLen)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(int16(clampF(s)*32767)))
	}
	_, err = f.Write(buf)
	return err
}

// readWAVFile reads a PCM WAV file (8/16/32-bit int or 32-bit float) and returns
// mono float32 samples and the sample rate. Multi-channel audio is downmixed.
func readWAVFile(path string) ([]float32, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a WAV file")
	}
	var (
		sampleRate, channels, bits, format int
		data                               []byte
	)
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		if body+size > len(b) {
			size = len(b) - body
		}
		switch id {
		case "fmt ":
			format = int(binary.LittleEndian.Uint16(b[body : body+2]))
			channels = int(binary.LittleEndian.Uint16(b[body+2 : body+4]))
			sampleRate = int(binary.LittleEndian.Uint32(b[body+4 : body+8]))
			bits = int(binary.LittleEndian.Uint16(b[body+14 : body+16]))
		case "data":
			data = b[body : body+size]
		}
		off = body + size + (size & 1) // chunks are word-aligned
	}
	if sampleRate == 0 || channels == 0 || data == nil {
		return nil, 0, fmt.Errorf("malformed WAV (rate=%d ch=%d)", sampleRate, channels)
	}

	bytesPerSample := bits / 8
	frame := bytesPerSample * channels
	if frame == 0 {
		return nil, 0, fmt.Errorf("invalid WAV sample size")
	}
	n := len(data) / frame
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		var sum float32
		for c := 0; c < channels; c++ {
			off := i*frame + c*bytesPerSample
			sum += sampleToFloat(data[off:off+bytesPerSample], bits, format)
		}
		out[i] = sum / float32(channels)
	}
	return out, sampleRate, nil
}

func sampleToFloat(b []byte, bits, format int) float32 {
	const fFloat = 3
	switch {
	case format == fFloat && bits == 32:
		return math.Float32frombits(binary.LittleEndian.Uint32(b))
	case bits == 16:
		return float32(int16(binary.LittleEndian.Uint16(b))) / 32768
	case bits == 32:
		return float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648
	case bits == 8:
		return (float32(b[0]) - 128) / 128
	}
	return 0
}

func clampF(s float32) float32 {
	if s > 1 {
		return 1
	}
	if s < -1 {
		return -1
	}
	return s
}
