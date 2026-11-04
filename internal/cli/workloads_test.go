package cli

import "testing"

func TestExpandVars(t *testing.T) {
	env := map[string]string{"URL": "http://x", "N": "3"}
	get := func(k string) string { return env[k] }
	in := `a: ${URL}
cmd: sh -c 'echo $HOME $((OGM_REPLICA_INDEX*10+1)) $(date) ${N} $${KEEP} ${MISSING}'`
	want := `a: http://x
cmd: sh -c 'echo $HOME $((OGM_REPLICA_INDEX*10+1)) $(date) 3 ${KEEP} '`
	if got := ExpandVars(in, get); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
