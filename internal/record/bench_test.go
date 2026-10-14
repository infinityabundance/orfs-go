package record

import (
	"bytes"
	"testing"
)

var benchSink []byte

func BenchmarkEncodeDataExtent4K(b *testing.B) {
	data := make([]byte, 4096)
	de := DataExtent{Offset: 0, Data: data}
	b.SetBytes(4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink = de.Encode()
	}
}

func BenchmarkDecodeDataExtent4K(b *testing.B) {
	data := make([]byte, 4096)
	enc := DataExtent{Offset: 0, Data: data}.Encode()
	b.SetBytes(4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeDataExtent(enc); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeFrame4K(b *testing.B) {
	payload := DataExtent{Offset: 0, Data: make([]byte, 4096)}.Encode()
	r := &Record{Header: Header{FormatVersion: FormatVersion, RecordType: TypeDataExtent, PayloadLength: uint64(len(payload)), InodeID: 5}, Payload: payload}
	enc, err := r.Encode()
	if err != nil {
		b.Fatal(err)
	}
	br := bytes.NewReader(enc)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := ReadFrameAt(br, 0, int64(len(enc))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCRC32C4K(b *testing.B) {
	buf := make([]byte, 4096)
	b.SetBytes(4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CRC32C(buf)
	}
}
