package termstatus

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	rtest "github.com/restic/restic/internal/test"
	"golang.org/x/sync/errgroup"
)

func TestStdioWrapper(t *testing.T) {
	var tests = []struct {
		inputs [][]byte
		output string
	}{
		{
			inputs: [][]byte{
				[]byte("foo"),
			},
			output: "foo\n",
		},
		{
			inputs: [][]byte{
				[]byte("foo"),
				[]byte("bar"),
				[]byte("\n"),
				[]byte("baz"),
			},
			output: "foobar\n" +
				"baz\n",
		},
		{
			inputs: [][]byte{
				[]byte("foo"),
				[]byte("bar\nbaz\n"),
				[]byte("bump\n"),
			},
			output: "foobar\n" +
				"baz\n" +
				"bump\n",
		},
		{
			inputs: [][]byte{
				[]byte("foo"),
				[]byte("bar\nbaz\n"),
				[]byte("bum"),
				[]byte("p\nx"),
				[]byte("x"),
				[]byte("x"),
				[]byte("z"),
			},
			output: "foobar\n" +
				"baz\n" +
				"bump\n" +
				"xxxz\n",
		},
	}

	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			var output strings.Builder
			w := newLineWriter(func(s string) { output.WriteString(s) })

			for _, data := range test.inputs {
				n, err := w.Write(data)
				if err != nil {
					t.Fatal(err)
				}

				if n != len(data) {
					t.Errorf("invalid length returned by Write, want %d, got %d", len(data), n)
				}
			}

			err := w.Close()
			if err != nil {
				t.Fatal(err)
			}

			if outstr := output.String(); outstr != test.output {
				t.Error(cmp.Diff(test.output, outstr))
			}
		})
	}
}

func TestStdioWrapperConcurrentWrites(t *testing.T) {
	// tests for race conditions when run with `go test -race ./internal/ui/termstatus`
	w := newLineWriter(func(_ string) {})

	wg, _ := errgroup.WithContext(context.TODO())
	for range 5 {
		wg.Go(func() error {
			_, err := w.Write([]byte("test\n"))
			return err
		})
	}
	rtest.OK(t, wg.Wait())
	rtest.OK(t, w.Close())
}
