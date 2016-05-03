package github

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"testing"

	"github.com/franela/goblin"
)

func TestHook(t *testing.T) {
	var (
		github client
		r      *http.Request
		body   *bytes.Buffer
	)

	g := goblin.Goblin(t)

	g.Describe("Hook", func() {
		g.BeforeEach(func() {
			github = client{}
			body = bytes.NewBuffer([]byte{})
			r, _ = http.NewRequest("POST", "https://drone.com/hook", body)
		})

		g.Describe("For a Pull Request", func() {
			g.BeforeEach(func() {
				r.Header.Set("X-Github-Event", "pull_request")
			})

			g.It("Should set build author to the pull request author", func() {
				hookJSON, ioerr := ioutil.ReadFile("fixtures/pull_request.json")
				if ioerr != nil {
					panic(ioerr)
				}
				body.Write(hookJSON)

				_, build, err := github.Hook(r)
				g.Assert(err).Equal(nil)
				g.Assert(build.Author).Equal("author")
				g.Assert(build.Avatar).Equal("https://avatars.githubusercontent.com/u/55555?v=3")
			})
		})
	})
}

//
// func TestLoad(t *testing.T) {
// 	conf := "https://github.com?client_id=client&client_secret=secret&scope=scope1,scope2"
//
// 	g := Load(conf)
// 	if g.URL != "https://github.com" {
// 		t.Errorf("g.URL = %q; want https://github.com", g.URL)
// 	}
// 	if g.Client != "client" {
// 		t.Errorf("g.Client = %q; want client", g.Client)
// 	}
// 	if g.Secret != "secret" {
// 		t.Errorf("g.Secret = %q; want secret", g.Secret)
// 	}
// 	if g.Scope != "scope1,scope2" {
// 		t.Errorf("g.Scope = %q; want scope1,scope2", g.Scope)
// 	}
// 	if g.API != DefaultAPI {
// 		t.Errorf("g.API = %q; want %q", g.API, DefaultAPI)
// 	}
// 	if g.MergeRef != DefaultMergeRef {
// 		t.Errorf("g.MergeRef = %q; want %q", g.MergeRef, DefaultMergeRef)
// 	}
//
// 	g = Load("")
// 	if g.Scope != DefaultScope {
// 		t.Errorf("g.Scope = %q; want %q", g.Scope, DefaultScope)
// 	}
// }
