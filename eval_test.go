package zoekt

import (
	"strconv"
	"testing"

	"github.com/google/zoekt/query"
)

// qgot prevents compiler optimizing away return value of simplifyRepo
var qgot query.Q

func BenchmarkSimplifyRepo(b *testing.B) {
	q, err := query.Parse("test")
	if err != nil {
		b.Fatal(err)
	}
	var reposQ []query.Q
	for i := 0; i < 2018; i++ {
		reposQ = append(reposQ, &query.Repo{Pattern: "github.com/foo/bar-" + strconv.Itoa(i)})
	}
	q = query.Simplify(query.NewAnd(q, query.NewOr(reposQ...)))

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		qgot = simplifyRepo(q, "github.com/foo/bar-100")
	}
}

func TestSimplifyRepo(t *testing.T) {
	q, err := query.Parse("test")
	if err != nil {
		t.Fatal(err)
	}
	want := q.String()
	var reposQ []query.Q
	for i := 0; i < 2018; i++ {
		reposQ = append(reposQ, &query.Repo{Pattern: "github.com/foo/bar-" + strconv.Itoa(i)})
	}
	q = query.Simplify(query.NewAnd(q, query.NewOr(reposQ...)))

	got := simplifyRepo(q, "github.com/foo/bar-100")
	if got.String() != want {
		t.Fatalf("got %v != %v", got.String(), want)
	}

	want = (&query.Const{Value: false}).String()
	got = simplifyRepo(q, "github.com/foo/baz")
	if got.String() != want {
		t.Fatalf("got %v != %v", got.String(), want)
	}
}
