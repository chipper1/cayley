// Copyright 2014 The Cayley Authors. All rights reserved.
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

package gremlin

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/robertkrimen/otto"
	_ "github.com/robertkrimen/otto/underscore"

	"github.com/google/cayley/graph"
	"github.com/google/cayley/query"
)

var ErrKillTimeout = errors.New("query timed out")

type Session struct {
	ts graph.TripleStore

	wk      *worker
	script  *otto.Script
	persist *otto.Otto

	timeout time.Duration
	kill    chan struct{}

	debug      bool
	dataOutput []interface{}

	err error
}

func NewSession(ts graph.TripleStore, timeout time.Duration, persist bool) *Session {
	g := Session{
		ts:      ts,
		wk:      newWorker(ts),
		timeout: timeout,
	}
	if persist {
		g.persist = g.wk.env
	}
	return &g
}

type Result struct {
	metaresult    bool
	err           error
	val           *otto.Value
	actualResults map[string]graph.Value
}

func (s *Session) ToggleDebug() {
	s.debug = !s.debug
}

func (s *Session) GetQuery(input string, out chan map[string]interface{}) {
	defer close(out)
	s.wk.shape = make(map[string]interface{})
	s.wk.env.Run(input)
	out <- s.wk.shape
	s.wk.shape = nil
}

func (s *Session) InputParses(input string) (query.ParseResult, error) {
	script, err := s.wk.env.Compile("", input)
	if err != nil {
		return query.ParseFail, err
	}
	s.script = script
	return query.Parsed, nil
}

func (s *Session) runUnsafe(input interface{}) (otto.Value, error) {
	wk := s.wk
	defer func() {
		if r := recover(); r != nil {
			if r == ErrKillTimeout {
				s.err = ErrKillTimeout
				wk.env = s.persist
				return
			}
			panic(r)
		}
	}()

	// Use buffered chan to prevent blocking.
	wk.env.Interrupt = make(chan func(), 1)
	s.kill = make(chan struct{})
	wk.kill = s.kill

	done := make(chan struct{})
	defer close(done)
	if s.timeout >= 0 {
		go func() {
			time.Sleep(s.timeout)
			select {
			case <-done:
			default:
				close(s.kill)
				wk.Lock()
				if wk.env != nil {
					wk.env.Interrupt <- func() {
						panic(ErrKillTimeout)
					}
				}
				wk.Unlock()
			}
		}()
	}

	wk.Lock()
	env := wk.env
	wk.Unlock()
	return env.Run(input)
}

func (s *Session) ExecInput(input string, out chan interface{}, _ int) {
	defer close(out)
	s.err = nil
	s.wk.results = out
	var err error
	var value otto.Value
	if s.script == nil {
		value, err = s.runUnsafe(input)
	} else {
		value, err = s.runUnsafe(s.script)
	}
	out <- &Result{
		metaresult: true,
		err:        err,
		val:        &value,
	}
	s.wk.results = nil
	s.script = nil
	s.wk.Lock()
	s.wk.env = s.persist
	s.wk.Unlock()
}

func (s *Session) ToText(result interface{}) string {
	data := result.(*Result)
	if data.metaresult {
		if data.err != nil {
			return fmt.Sprintf("Error: %v\n", data.err)
		}
		if data.val != nil {
			s, _ := data.val.Export()
			if data.val.IsObject() {
				typeVal, _ := data.val.Object().Get("_gremlin_type")
				if !typeVal.IsUndefined() {
					s = "[internal Iterator]"
				}
			}
			return fmt.Sprintln("=>", s)
		}
		return ""
	}
	var out string
	out = fmt.Sprintln("****")
	if data.val == nil {
		tags := data.actualResults
		tagKeys := make([]string, len(tags))
		i := 0
		for k, _ := range tags {
			tagKeys[i] = k
			i++
		}
		sort.Strings(tagKeys)
		for _, k := range tagKeys {
			if k == "$_" {
				continue
			}
			out += fmt.Sprintf("%s : %s\n", k, s.ts.NameOf(tags[k]))
		}
	} else {
		if data.val.IsObject() {
			export, _ := data.val.Export()
			mapExport := export.(map[string]string)
			for k, v := range mapExport {
				out += fmt.Sprintf("%s : %v\n", k, v)
			}
		} else {
			strVersion, _ := data.val.ToString()
			out += fmt.Sprintf("%s\n", strVersion)
		}
	}
	return out
}

// Web stuff
func (s *Session) BuildJson(result interface{}) {
	data := result.(*Result)
	if !data.metaresult {
		if data.val == nil {
			obj := make(map[string]string)
			tags := data.actualResults
			tagKeys := make([]string, len(tags))
			i := 0
			for k, _ := range tags {
				tagKeys[i] = k
				i++
			}
			sort.Strings(tagKeys)
			for _, k := range tagKeys {
				obj[k] = s.ts.NameOf(tags[k])
			}
			s.dataOutput = append(s.dataOutput, obj)
		} else {
			if data.val.IsObject() {
				export, _ := data.val.Export()
				s.dataOutput = append(s.dataOutput, export)
			} else {
				strVersion, _ := data.val.ToString()
				s.dataOutput = append(s.dataOutput, strVersion)
			}
		}
	}
}

func (s *Session) GetJson() ([]interface{}, error) {
	defer s.ClearJson()
	if s.err != nil {
		return nil, s.err
	}
	select {
	case <-s.kill:
		return nil, ErrKillTimeout
	default:
		return s.dataOutput, nil
	}
}

func (s *Session) ClearJson() {
	s.dataOutput = nil
}
