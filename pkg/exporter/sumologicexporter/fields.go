// Copyright 2020 OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sumologicexporter

import (
	"sort"
	"strings"

	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/collector/model/pdata"
)

// fields represents metadata
type fields struct {
	orig pdata.AttributeMap
}

func newFields(attrMap pdata.AttributeMap) fields {
	return fields{
		orig: attrMap,
	}
}

func (f fields) isEmpty() bool {
	return f.orig.Len() == 0
}

func (f fields) equals(other fields) bool {
	if f.orig.Len() != other.orig.Len() {
		return false
	}

	return cmp.Equal(f.orig.AsRaw(), other.orig.AsRaw())
}

// string returns fields as ordered key=value string with `, ` as separator
func (f fields) string() string {
	returnValue := make([]string, 0, f.orig.Len())

	f.orig.Range(func(k string, v pdata.AttributeValue) bool {
		// Don't add source related attributes to fields as they are handled separately
		// and are added to the payload either as special HTTP headers or as resources
		// attributes.
		if k == attributeKeySourceCategory || k == attributeKeySourceHost || k == attributeKeySourceName {
			return true
		}

		sv := v.AsString()

		// Skip empty field
		if len(sv) == 0 {
			return true
		}

		key := []byte(k)
		f.sanitizeField(key)
		value := []byte(sv)
		f.sanitizeField(value)
		sb := strings.Builder{}
		sb.Grow(len(key) + len(value) + 1)
		sb.Write(key)
		sb.WriteRune('=')
		sb.Write(value)

		returnValue = append(
			returnValue,
			sb.String(),
		)
		return true
	})
	sort.Strings(returnValue)

	return strings.Join(returnValue, ", ")
}

// sanitizeFields sanitize field (key or value) to be correctly parsed by sumologic receiver
// It modifies the field in place.
func (f fields) sanitizeField(fld []byte) {
	for i := 0; i < len(fld); i++ {
		switch fld[i] {
		case ',':
			fld[i] = '_'
		case '=':
			fld[i] = ':'
		case '\n':
			fld[i] = '_'
		default:
		}
	}
}

// translateAttributes translates fields to sumo format
func (f *fields) translateAttributes() {
	f.orig = translateAttributes(f.orig)
}
