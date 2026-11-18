// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package runtimev1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// Fingerprint hashes every message field, enum value and RPC in fd — names,
// numbers, types, cardinality and oneof membership — so any change a runtime
// implementor would see moves it, while comments and ordering do not.
func Fingerprint(fd protoreflect.FileDescriptor) string {
	var lines []string
	walkEnums := func(es protoreflect.EnumDescriptors) {
		for i := 0; i < es.Len(); i++ {
			vs := es.Get(i).Values()
			for j := 0; j < vs.Len(); j++ {
				lines = append(lines, fmt.Sprintf("enum %s=%d", vs.Get(j).FullName(), vs.Get(j).Number()))
			}
		}
	}
	var walkMsgs func(protoreflect.MessageDescriptors)
	walkMsgs = func(ms protoreflect.MessageDescriptors) {
		for i := 0; i < ms.Len(); i++ {
			m := ms.Get(i)
			lines = append(lines, "message "+string(m.FullName()))
			for j := 0; j < m.Fields().Len(); j++ {
				lines = append(lines, fieldLine(m.Fields().Get(j)))
			}
			walkEnums(m.Enums())
			walkMsgs(m.Messages())
		}
	}
	walkMsgs(fd.Messages())
	walkEnums(fd.Enums())
	for i := 0; i < fd.Services().Len(); i++ {
		ms := fd.Services().Get(i).Methods()
		for j := 0; j < ms.Len(); j++ {
			m := ms.Get(j)
			lines = append(lines, fmt.Sprintf("rpc %s(%s,%t)->(%s,%t)", m.FullName(),
				m.Input().FullName(), m.IsStreamingClient(), m.Output().FullName(), m.IsStreamingServer()))
		}
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8])
}

func fieldLine(f protoreflect.FieldDescriptor) string {
	typ := f.Kind().String()
	if md := f.Message(); md != nil {
		typ = string(md.FullName())
	} else if ed := f.Enum(); ed != nil {
		typ = string(ed.FullName())
	}
	oneof := ""
	if o := f.ContainingOneof(); o != nil && !o.IsSynthetic() {
		oneof = string(o.Name())
	}
	return fmt.Sprintf("field %s=%d %s %s optional=%t oneof=%s",
		f.FullName(), f.Number(), f.Cardinality(), typ, f.HasOptionalKeyword(), oneof)
}
