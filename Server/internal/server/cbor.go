package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
)

// decodeCBOR decodes the subset of CBOR that App Attest uses: unsigned and negative integers,
// byte and text strings, arrays and maps (definite lengths only). Maps decode to
// map[string]any; byte strings to []byte.
func decodeCBOR(data []byte) (any, error) {
	v, n, err := decodeCBORPrefix(data)
	if err != nil {
		return nil, err
	}
	if n != len(data) {
		return nil, errors.New("cbor: trailing bytes")
	}
	return v, nil
}

// decodeCBORPrefix decodes one value from the start of data and says how many bytes it used.
func decodeCBORPrefix(data []byte) (any, int, error) {
	d := cborDecoder{data: data}
	v, err := d.value(0)
	return v, d.pos, err
}

type cborDecoder struct {
	data []byte
	pos  int
}

func (d *cborDecoder) head() (major byte, arg uint64, err error) {
	if d.pos >= len(d.data) {
		return 0, 0, errors.New("cbor: unexpected end")
	}
	b := d.data[d.pos]
	d.pos++
	major, info := b>>5, b&0x1f
	switch {
	case info < 24:
		return major, uint64(info), nil
	case info <= 27:
		n := 1 << (info - 24)
		if d.pos+n > len(d.data) {
			return 0, 0, errors.New("cbor: unexpected end")
		}
		buf := d.data[d.pos : d.pos+n]
		d.pos += n
		switch n {
		case 1:
			arg = uint64(buf[0])
		case 2:
			arg = uint64(binary.BigEndian.Uint16(buf))
		case 4:
			arg = uint64(binary.BigEndian.Uint32(buf))
		default:
			arg = binary.BigEndian.Uint64(buf)
		}
		return major, arg, nil
	default:
		return 0, 0, fmt.Errorf("cbor: unsupported additional info %d", info)
	}
}

func (d *cborDecoder) value(depth int) (any, error) {
	if depth > 16 {
		return nil, errors.New("cbor: nested too deeply")
	}
	major, arg, err := d.head()
	if err != nil {
		return nil, err
	}
	switch major {
	case 0:
		return arg, nil
	case 1:
		return -1 - int64(arg), nil
	case 2, 3:
		if arg > uint64(len(d.data)-d.pos) {
			return nil, errors.New("cbor: string runs past the end")
		}
		b := d.data[d.pos : d.pos+int(arg)]
		d.pos += int(arg)
		if major == 3 {
			return string(b), nil
		}
		return append([]byte{}, b...), nil
	case 4:
		if arg > uint64(len(d.data)) {
			return nil, errors.New("cbor: array too long")
		}
		out := make([]any, 0, arg)
		for i := uint64(0); i < arg; i++ {
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case 5:
		if arg > uint64(len(d.data)) {
			return nil, errors.New("cbor: map too long")
		}
		out := make(map[string]any, arg)
		for i := uint64(0); i < arg; i++ {
			k, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			var key string
			switch k := k.(type) {
			case string:
				key = k
			case uint64: // COSE keys use integer labels: 1, 3, -1, -2...
				key = strconv.FormatUint(k, 10)
			case int64:
				key = strconv.FormatInt(k, 10)
			default:
				return nil, errors.New("cbor: unsupported map key")
			}
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
		return out, nil
	default:
		return nil, fmt.Errorf("cbor: unsupported major type %d", major)
	}
}
