// Copyright 2018 The Terraformer Authors.
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

package terraform_utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/hcl/hcl/ast"
	hcl_printer "github.com/hashicorp/hcl/hcl/printer"
	hcl_parcer "github.com/hashicorp/hcl/json/parser"
)

// Copy code from https://github.com/kubernetes/kops project with few changes for support many provider and heredoc

const safeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"

// sanitizer fixes up an invalid HCL AST, as produced by the HCL parser for JSON
type astSanitizer struct{}

// output prints creates b printable HCL output and returns it.
func (v *astSanitizer) visit(n interface{}) {
	switch t := n.(type) {
	case *ast.File:
		v.visit(t.Node)
	case *ast.ObjectList:
		var index int
		for {
			if index == len(t.Items) {
				break
			}
			v.visit(t.Items[index])
			index++
		}
	case *ast.ObjectKey:
	case *ast.ObjectItem:
		v.visitObjectItem(t)
	case *ast.LiteralType:
	case *ast.ListType:
	case *ast.ObjectType:
		v.visit(t.List)
	default:
		fmt.Printf(" unknown type: %T\n", n)
	}

}

func (v *astSanitizer) visitObjectItem(o *ast.ObjectItem) {
	for i, k := range o.Keys {
		if i == 0 {
			text := k.Token.Text
			if text != "" && text[0] == '"' && text[len(text)-1] == '"' {
				v := text[1 : len(text)-1]
				safe := true
				for _, c := range v {
					if strings.IndexRune(safeChars, c) == -1 {
						safe = false
						break
					}
				}
				if safe {
					k.Token.Text = v
				}
			}
		}
	}
	switch t := o.Val.(type) {
	case *ast.LiteralType: // heredoc support
		if strings.HasPrefix(t.Token.Text, `"<<`) {
			t.Token.Text = t.Token.Text[1:]
			t.Token.Text = t.Token.Text[:len(t.Token.Text)-1]
			t.Token.Text = strings.Replace(t.Token.Text, `\n`, "\n", -1)
			t.Token.Text = strings.Replace(t.Token.Text, `\t`, "", -1)
			t.Token.Type = 10
			// check if text json for Unquote and Indent
			tmp := map[string]interface{}{}
			jsonTest := t.Token.Text
			lines := strings.Split(jsonTest, "\n")
			jsonTest = strings.Join(lines[1:len(lines)-1], "\n")
			jsonTest = strings.Replace(jsonTest, "\\\"", "\"", -1)
			// it's json we convert to heredoc back
			err := json.Unmarshal([]byte(jsonTest), &tmp)
			if err == nil {
				dataJsonBytes, err := json.MarshalIndent(tmp, "", "  ")
				if err == nil {
					jsonData := strings.Split(string(dataJsonBytes), "\n")
					// first line for heredoc
					jsonData = append([]string{lines[0]}, jsonData...)
					// last line for heredoc
					jsonData = append(jsonData, lines[len(lines)-1])
					hereDoc := strings.Join(jsonData, "\n")
					t.Token.Text = hereDoc
				}
			}
		}
	default:
	}

	// A hack so that Assign.IsValid is true, so that the printer will output =
	o.Assign.Line = 1

	v.visit(o.Val)
}

func hclPrint(node ast.Node) ([]byte, error) {
	var sanitizer astSanitizer
	sanitizer.visit(node)

	var b bytes.Buffer
	err := hcl_printer.Fprint(&b, node)
	if err != nil {
		return nil, fmt.Errorf("error writing HCL: %v", err)
	}
	s := b.String()

	// Remove extra whitespace...
	s = strings.Replace(s, "\n\n", "\n", -1)

	// ...but leave whitespace between resources
	s = strings.Replace(s, "}\nresource", "}\n\nresource", -1)

	// Workaround HCL insanity #6359: quotes are _not_ escaped in quotes
	// This hits the file function
	s = strings.Replace(s, "(\\\"", "(\"", -1)
	s = strings.Replace(s, "\\\")", "\")", -1)

	// We don't need to escape > or <
	s = strings.Replace(s, "\\u003c", "<", -1)
	s = strings.Replace(s, "\\u003e", ">", -1)

	// Apply Terraform style (alignment etc.)
	formatted, err := hcl_printer.Format([]byte(s))
	if err != nil {
		log.Println("Invalid HCL follows:")
		for i, line := range strings.Split(s, "\n") {
			fmt.Printf("%d\t%s", i+1, line)
		}
		return nil, fmt.Errorf("error formatting HCL: %v", err)
	}

	return formatted, nil
}

// Sanitize name for terraform style
func TfSanitize(name string) string {
	name = strings.Replace(name, "*.", "", -1)
	name = strings.Replace(name, ".", "-", -1)
	name = strings.Replace(name, "/", "--", -1)
	return name
}

// Print hcl file from TerraformResource + provider
func HclPrint(resources []TerraformResource, provider map[string]interface{}) ([]byte, error) {
	resourcesByType := map[string]map[string]interface{}{}

	for _, res := range resources {
		r := resourcesByType[res.ResourceType]
		if r == nil {
			r = make(map[string]interface{})
			resourcesByType[res.ResourceType] = r
		}

		tfName := TfSanitize(res.ResourceName)

		if r[tfName] != nil {
			return []byte{}, fmt.Errorf("duplicate resource found: %s.%s", res.ResourceType, tfName)
		}

		r[tfName] = res.Item
	}

	data := map[string]interface{}{}
	data["resource"] = resourcesByType
	data["provider"] = provider

	var err error
	dataJsonBytes, err := json.MarshalIndent(data, "", "  ")
	dataJson := string(dataJsonBytes)
	dataJson = strings.Replace(dataJson, "\\u003c", "<", -1)
	if err != nil {
		return []byte{}, fmt.Errorf("error marshalling terraform data to json: %v", err)
	}
	nodes, err := hcl_parcer.Parse([]byte(dataJson))
	if err != nil {
		return []byte{}, fmt.Errorf("error parsing terraform json: %v", err)
	}
	hclBytes, err := hclPrint(nodes)
	if err != nil {
		return []byte{}, err
	}
	return hclBytes, nil
}