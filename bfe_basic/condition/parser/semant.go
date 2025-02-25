// Copyright (c) 2019 Baidu, Inc.
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

// semant check for condition expression 

package parser

import (
	"fmt"
	"log"
)

// funcProtos holds a mapping from func name to args types.
var funcProtos = map[string][]Token{
	"default_t":                  nil,
	"req_cip_trusted":            nil,
	"req_vip_in":                 []Token{STRING},
	"req_proto_match":            []Token{STRING},
	"req_proto_secure":           nil,
	"req_host_in":                []Token{STRING},
	"req_host_regmatch":          []Token{STRING},
	"req_path_in":                []Token{STRING, BOOL},
	"req_path_prefix_in":         []Token{STRING, BOOL},
	"req_path_suffix_in":         []Token{STRING, BOOL},
	"req_path_regmatch":          []Token{STRING},
	"req_query_key_prefix_in":    []Token{STRING},
	"req_query_key_in":           []Token{STRING},
	"req_query_exist":            nil,
	"req_query_value_in":         []Token{STRING, STRING, BOOL},
	"req_query_value_prefix_in":  []Token{STRING, STRING, BOOL},
	"req_query_value_suffix_in":  []Token{STRING, STRING, BOOL},
	"req_query_value_regmatch":   []Token{STRING, STRING},
	"req_url_regmatch":           []Token{STRING},
	"req_cookie_key_in":          []Token{STRING},
	"req_cookie_value_in":        []Token{STRING, STRING, BOOL},
	"req_cookie_value_prefix_in": []Token{STRING, STRING, BOOL},
	"req_cookie_value_suffix_in": []Token{STRING, STRING, BOOL},
	"req_port_in":                []Token{STRING},
	"req_tag_match":              []Token{STRING, STRING},
	"req_ua_regmatch":            []Token{STRING},
	"req_header_key_in":          []Token{STRING},
	"req_header_value_in":        []Token{STRING, STRING, BOOL},
	"req_header_value_prefix_in": []Token{STRING, STRING, BOOL},
	"req_header_value_suffix_in": []Token{STRING, STRING, BOOL},
	"req_header_value_regmatch":  []Token{STRING, STRING},
	"req_method_in":              []Token{STRING},
	"req_cip_range":              []Token{STRING, STRING},
	"req_vip_range":              []Token{STRING, STRING},
	"res_code_in":                []Token{STRING},
	"res_header_key_in":          []Token{STRING},
	"res_header_value_in":        []Token{STRING, STRING, BOOL},
	"ses_vip_range":              []Token{STRING, STRING},
	"ses_sip_range":              []Token{STRING, STRING},
}

func prototypeCheck(expr *CallExpr) error {
	// log.Printf("start prototype Check")
	argsType, ok := funcProtos[expr.Fun.Name]
	if !ok {
		return fmt.Errorf("primitive %s not found", expr.Fun.Name)
	}

	if len(argsType) != len(expr.Args) {
		return fmt.Errorf("primitive args len error, expect %v, got %v", len(argsType), len(expr.Args))
	}

	for i, argType := range argsType {
		if argType != expr.Args[i].Kind {
			return fmt.Errorf("primitive %s arg %d expect %s, got %s",
				expr.Fun.Name, i, argType, expr.Args[i].Kind)
		}
	}

	return nil
}

// primitiveCheck is a traverse function to check all func call prototype.
// check: 1. func name 2. func args len and type
func (p *Parser) primitiveCheck(n Node) bool {
	switch x := n.(type) {
	case *BinaryExpr, *UnaryExpr, *ParenExpr:
		return true
	case *Ident:
		return false
	case *CallExpr:
		if err := prototypeCheck(x); err != nil {
			p.addError(x.Pos(), err.Error())
		}
		return false
	default:
		log.Printf("get a node %s", n)
	}

	return false
}

// collectVariable is a traverse function to collect all variables(Ident) from nodeTree.
func (p *Parser) collectVariable(n Node) bool {
	switch x := n.(type) {
	case *BinaryExpr, *UnaryExpr, *ParenExpr:
		return true
	case *Ident:
		exist := false
		for _, i := range p.identList {
			if i.Name == x.Name {
				exist = true
			}
		}

		if !exist {
			p.identList = append(p.identList, x)
		}
	case *CallExpr:
		return false
	default:
		return false
	}

	return false
}
