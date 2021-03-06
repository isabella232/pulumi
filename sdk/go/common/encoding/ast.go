// Copyright 2016-2021, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// nolint: goconst
package encoding

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/pkg/errors"
	"github.com/pulumi/go-yaml/ast"
	"github.com/pulumi/go-yaml/parser"
	"github.com/pulumi/go-yaml/printer"
	"github.com/pulumi/go-yaml/token"
	"github.com/pulumi/pulumi/sdk/v2/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v2/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v2/go/common/util/contract"
)

const indentSpaces = 2 // Set the indent size for the YAML doc generated by the FileAST

// FileAST manages an Abstract Syntax Tree (AST) for YAML configuration files.
type FileAST struct {
	ast *ast.File
}

// NewFileAST initializes the AST from the given input bytes.
func NewFileAST(yamlBytes []byte) (*FileAST, error) {
	if yamlBytes == nil {
		return &FileAST{}, nil
	}

	fileAST, err := parser.ParseBytes(yamlBytes, parser.ParseComments)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse YAML file")
	}

	return &FileAST{ast: fileAST}, nil
}

// IsEmpty checks for an uninitialized or empty AST.
func (f *FileAST) IsEmpty() bool {
	return f.ast == nil || len(f.ast.Docs) == 0
}

// HasKey returns true if the key exists on the root AST document, false otherwise.
func (f *FileAST) HasKey(k string) bool {
	if f.IsEmpty() {
		return false
	}
	node := f.ast.Docs[0].Body.(*ast.MappingNode)
	for _, v := range node.Values {
		if v.Key.String() == k {
			return true
		}
	}
	return false
}

// Marshal converts the AST to YAML.
func (f *FileAST) Marshal() []byte {
	out := bytes.Buffer{}
	var p printer.Printer
	for _, d := range f.ast.Docs {
		out.Write(p.PrintNode(d))
	}

	return out.Bytes()
}

// SetConfig sets the value for a given key. If path is true, the key's name portion is treated as a path.
func (f *FileAST) SetConfig(rootKey string, key config.Key, value config.Value, path bool) error {
	if f.IsEmpty() {
		return fmt.Errorf("tried to set an empty config")
	}

	// Get the rootKey node.
	node := f.ast.Docs[0].Body.(*ast.MappingNode)
	var err error
	if len(rootKey) > 0 {
		node, err = walk(node, rootKey)
		if err != nil {
			return errors.Wrapf(err, "failed to walk to rootKey: %q", rootKey)
		}
	}

	// If the key isn't a path, go ahead and set the value and return.
	if !path {
		if len(node.Values) == 0 && node.IsFlowStyle {
			node.SetIsFlowStyle(false)
			node.GetToken().Position.Column += indentSpaces
		}
		node = upsertNode(node, key.String(),
			newValueNode(value.RawValue(), value.Secure(), node.GetToken().Position.Column))
		return nil
	}

	// Otherwise, parse the path and get the new config key.
	pathSegments, configKey, err := config.ParseKeyPath(key)
	if err != nil {
		return err
	}

	// If we only have a single path segment, set the value and return.
	if len(pathSegments) == 1 {
		node = upsertNode(node, configKey.String(),
			newValueNode(value.RawValue(), value.Secure(), node.GetToken().Position.Column))
		return nil
	}

	var parent ast.Node
	var parentKey interface{}
	var cursor ast.Node
	var cursorKey interface{}
	cursor = node
	cursorKey = configKey.String()
	for _, pkey := range pathSegments[1:] {
		column := cursor.GetToken().Position.Column
		pvalue, err := getConfigFromNode(cursor, cursorKey)
		if err != nil {
			return err
		}

		// If the value is nil or a simple value, create a new Node.
		// Otherwise, return an error due to the type mismatch.
		simpleValue := pvalue != nil && (pvalue.Type() == ast.StringType ||
			pvalue.Type() == ast.IntegerType ||
			pvalue.Type() == ast.LiteralType ||
			pvalue.Type() == ast.FloatType)
		// A secret value also counts as a simple value.
		if mn, ok := pvalue.(*ast.MappingNode); ok && len(mn.Values) == 1 && mn.Values[0].Key.String() == "secure" {
			simpleValue = true
		}
		var newValue ast.Node
		switch pkey.(type) {
		case int:
			if pvalue == nil || simpleValue {
				newValue = newSequenceNode(column)
			} else if _, ok := pvalue.(*ast.SequenceNode); !ok {
				return errors.Errorf("an array was expected for index %v", pkey)
			}
		case string:
			if pvalue == nil || simpleValue {
				newValue = newMappingNode(pkey.(string), column+indentSpaces)
			} else if _, ok := pvalue.(*ast.MappingNode); !ok {
				return errors.Errorf("a map was expected for key %q", pkey)
			}
		default:
			contract.Failf("unexpected path type")
		}
		if newValue != nil {
			pvalue = newValue
			cursor, err = setValue(cursorKey, parentKey, cursor, pvalue, parent)
			if err != nil {
				return err
			}
		}

		parent = cursor
		parentKey = cursorKey
		cursor = pvalue
		cursorKey = pkey
	}

	// Adjust the value (e.g. convert "true"/"false" to booleans and integers to ints) and set it.
	adjustedValue := newValueNode(
		config.AdjustObjectValue(value, path), value.Secure(), cursor.GetToken().Position.Column)
	if _, err = setValue(cursorKey, parentKey, cursor, adjustedValue, parent); err != nil {
		return err
	}

	// Secure values are reserved, so return an error when attempting to add one.
	if isSecureValue(cursor) {
		return errSecureKeyReserved
	}

	return nil
}

// RemoveConfig removes the value for a given key. If path is true, the key's name portion is treated as a path.
func (f *FileAST) RemoveConfig(rootKey string, k config.Key, path bool) error {
	if f.IsEmpty() {
		return nil
	}

	// Get the rootKey node.
	root := f.ast.Docs[0].Body.(*ast.MappingNode)
	var err error
	if len(rootKey) > 0 {
		root, err = walk(root, rootKey)
		if err != nil {
			return errors.Wrapf(err, "failed to walk to rootKey: %q", rootKey)
		}
	}

	// If the key isn't a path, delete the value and return.
	if !path {
		removeKey(root, k.String())
		return nil
	}

	// Parse the path.
	// Otherwise, parse the path and get the new config key.
	pathSegments, configKey, err := config.ParseKeyPath(k)
	if err != nil {
		return err
	}

	if len(pathSegments) == 0 {
		return nil
	}

	// If we only have a single path segment, delete the key and return.
	if len(pathSegments) == 1 {
		removeKey(root, configKey.String())
		return nil
	}

	// Get the value within the object up to the second-to-last path segment.
	// If not found, exit early.
	p := []interface{}{configKey.String()}
	if len(pathSegments) > 2 {
		p = append(p, pathSegments[1:len(pathSegments)-1]...)
	}

	parent, dest, ok := getNodeForPath(root, resource.PropertyPath(p))
	if !ok {
		return nil
	}

	// Remove the last path segment.
	key := pathSegments[len(pathSegments)-1]
	removeKey(dest, key)
	// Fix parent MappingNode if child is now empty.
	if t, ok := dest.(*ast.MappingNode); ok && len(t.Values) == 0 {
		if pt, ok := parent.(*ast.MappingNode); ok {
			var parentKey string
			if len(pathSegments) == 2 {
				parentKey = configKey.String()
			} else {
				parentKey = pathSegments[len(pathSegments)-2].(string)
			}
			parent = upsertNode(pt, parentKey, newMappingNode("-", parent.GetToken().Position.Column))
		}
	}

	return nil
}

var errSecureKeyReserved = errors.New(`"secure" key in maps of length 1 are reserved`)

// isSecureValue returns true if the node is a MappingNode with one value and a "secure" key.
func isSecureValue(v ast.Node) bool {
	if m, isMappingNode := v.(*ast.MappingNode); isMappingNode && len(m.Values) == 1 {
		return m.Values[0].Key.String() == "secure"
	}
	return false
}

// newMappingNode initializes a MappingNode (represents a YAML map).
func newMappingNode(key string, column int) *ast.MappingNode {
	k := token.New(key, key, &token.Position{Column: column})
	return &ast.MappingNode{
		BaseNode: &ast.BaseNode{},
		Start:    k,
		Values:   []*ast.MappingValueNode{},
	}
}

// newSequenceNode initializes a SequenceNode (represents a YAML sequence).
func newSequenceNode(column int) *ast.SequenceNode {
	return &ast.SequenceNode{
		BaseNode: &ast.BaseNode{},
		Start:    token.New("-", "-", &token.Position{Column: column}),
		Values:   []ast.Node{},
	}
}

// newMappingValueNode initializes a MappingValueNode (represents an entry in a YAML map or sequence).
func newMappingValueNode(key string, value ast.Node, column int) *ast.MappingValueNode {
	k := token.New(key, key, &token.Position{Column: column})
	return &ast.MappingValueNode{
		BaseNode: &ast.BaseNode{},
		Start:    k,
		Key:      ast.String(k),
		Value:    value,
	}
}

// upsertNode updates an entry in a MappingNode if it already exists, or appends it to the MappingNode otherwise.
func upsertNode(root *ast.MappingNode, key string, node ast.Node) *ast.MappingNode {
	// Special case for replacing a secret value.
	if len(root.Values) == 1 && root.Values[0].Key.String() == "secure" {
		root.Values[0] = newMappingValueNode(key, node, root.Values[0].GetToken().Position.Column)
		return root
	}

	for i, v := range root.Values {
		if v.Key.String() == key {
			root.Values[i].Value = node
			return root
		}
	}

	column := root.GetToken().Position.Column
	root.Values = append(root.Values, newMappingValueNode(key, node, column))
	return root
}

// getConfigFromNode retrieves the Node matching the provided key. The return values will be nil if the key does not
// exist in the Node, and an error will be returned if the provided key doesn't match the Node type.
func getConfigFromNode(node ast.Node, key interface{}) (ast.Node, error) {
	switch nodeT := node.(type) {
	case *ast.MappingNode:
		k, ok := key.(string)
		contract.Assertf(ok, "key for a map must be a string")

		for _, v := range nodeT.Values {
			if v.Key.String() == k {
				return v.Value, nil
			}
		}
		return nil, nil
	case *ast.SequenceNode:
		idx, ok := key.(int)
		contract.Assertf(ok, "key for an array must be an int")

		if idx < 0 || idx > len(nodeT.Values) {
			return nil, errors.New("array index out of range")
		}
		// We explicitly allow idx == len(t) here, which indicates a
		// value that will be appended to the end of the array.
		if idx == len(nodeT.Values) {
			return nil, nil
		}
		return nodeT.Values[idx], nil
	case *ast.MappingValueNode:
		return getConfigFromNode(nodeT.Value, key)
	default:
		contract.Failf("should not reach here")
		return nil, nil
	}
}

// newValueNode initializes a Node based on the input type.
func newValueNode(value interface{}, secure bool, column int) ast.Node {
	var v ast.Node
	switch t := value.(type) {
	case bool:
		s := strconv.FormatBool(t)
		v = ast.Bool(token.New(s, s, &token.Position{Column: column}))
	case float32, float64:
		s := fmt.Sprintf("%f", t)
		v = ast.Float(token.New(s, s, &token.Position{Column: column}))
	case int:
		s := strconv.Itoa(t)
		v = ast.Integer(token.New(s, s, &token.Position{Column: column}))
	case string:
		if len(t) > 0 && t[0] == '0' {
			v = ast.String(token.DoubleQuote(t, t, &token.Position{Column: column}))
		} else {
			v = ast.String(token.String(t, t, &token.Position{Column: column}))
		}
	case config.Value:
		v = ast.String(token.String(t.RawValue(), t.RawValue(), &token.Position{Column: column}))
	}

	if secure {
		secureToken := token.New("secure", "secure", &token.Position{Column: column + indentSpaces})
		return &ast.MappingValueNode{
			BaseNode: &ast.BaseNode{},
			Start:    secureToken,
			Key:      ast.String(secureToken),
			Value:    v,
		}
	}
	return v
}

// Set value sets the value in the container for the given key, and returns the container.
func setValue(key, containerParentKey interface{}, container, value, containerParent ast.Node) (ast.Node, error) {
	switch t := container.(type) {
	case *ast.MappingNode:
		k, ok := key.(string)
		contract.Assertf(ok, "key for a map must be a string")
		t = upsertNode(t, k, value)
	case *ast.SequenceNode:
		i, ok := key.(int)
		contract.Assertf(ok, "key for an array must be an int")
		// We allow i == len(t), which indicates the value should be appended to the end of the array.
		if i < 0 || i > len(t.Values) {
			return nil, errors.New("array index out of range")
		}
		// If i == len(t), we need to append to the end of the array, which involves creating a new slice
		// and saving it in the parent container.
		if i == len(t.Values) {
			t.Values = append(t.Values, value)
			contract.Assertf(containerParent != nil, "parent must not be nil")
			contract.Assertf(containerParentKey != nil, "parentKey must not be nil")
			return t, nil
		}
		t.Values[i] = value
	default:
		contract.Failf("unexpected container type: %T", container)
	}
	return container, nil
}

// removeKey deletes the config in a Node matching the provided key if one is present.
func removeKey(node ast.Node, key interface{}) {
	switch t := node.(type) {
	case *ast.MappingNode:
		k, ok := key.(string)
		if !ok {
			return
		}
		for i, v := range t.Values {
			if v.Key.String() == k {
				t.Values = append(t.Values[:i], t.Values[i+1:]...)
				return
			}
		}
	case *ast.SequenceNode:
		idx, ok := key.(int)
		if !ok || idx < 0 || idx >= len(t.Values) {
			return
		}
		t.Values = append(t.Values[:idx], t.Values[idx+1:]...)
		return
	}
	return
}

// getNodeForPath returns the parent, value, and true if the value is found in source given the path segments in p.
func getNodeForPath(source ast.Node, p resource.PropertyPath) (ast.Node, ast.Node, bool) {
	// If the source is nil, exit early.
	if source == nil {
		return nil, nil, false
	}

	// Lookup the value by each path segment.
	var parent ast.Node
	var err error
	v := source
	for _, key := range p {
		parent = v
		switch t := v.(type) {
		case *ast.MappingNode:
			k, ok := key.(string)
			if !ok {
				return nil, nil, false
			}
			v, err = getConfigFromNode(t, k)
			if err != nil {
				return nil, nil, false
			}
		case *ast.SequenceNode:
			index, ok := key.(int)
			if !ok || index < 0 || index >= len(t.Values) {
				return nil, nil, false
			}
			v, err = getConfigFromNode(t, index)
			if err != nil {
				return nil, nil, false
			}
		default:
			return nil, nil, false
		}
	}
	return parent, v, true
}

// walk traverses the node to the key. The key format supports dotted values for nested map values. An error is
// returned if the provided key is not found.
func walk(node *ast.MappingNode, key string) (*ast.MappingNode, error) {
	for _, v := range node.Values {
		if v.Key.String() == key {
			switch t := v.Value.(type) {
			case *ast.MappingNode:
				return t, nil
			}
		}
	}
	return nil, fmt.Errorf("config key not found: %q", key)
}
