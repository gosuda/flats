package cli

import (
	"fmt"
	"net/http"
)

func (a *app) network(args []string) error {
	if len(args) == 0 {
		return usagef("missing subcommand (ls, set or clear)")
	}
	sub, args := args[0], args[1:]
	fs := a.flags("network")
	min, max := 1, 1
	switch sub {
	case "ls", "list", "clear":
	case "set":
		min, max = 2, 33
	default:
		return usagef("unknown network subcommand %q (use ls, set or clear)", sub)
	}
	pos, err := a.parse(fs, args, min, max)
	if err != nil {
		return err
	}
	method := http.MethodGet
	var body any
	if sub == "set" || sub == "clear" {
		method = http.MethodPut
		origins := []string{}
		if sub == "set" {
			origins = pos[1:]
		}
		body = map[string]any{"origins": origins}
	}
	var out struct {
		Origins []string `json:"origins"`
		Note    string   `json:"note"`
	}
	resp, err := a.client().call(a.ctx, method, "/api/flats/"+pathEscape(pos[0])+"/network", nil, body, &out)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	if len(out.Origins) == 0 {
		fmt.Fprintln(a.out, "No server HTTP(S) origins permitted.")
	}
	for _, origin := range out.Origins {
		fmt.Fprintln(a.out, origin)
	}
	fmt.Fprintln(a.out, out.Note)
	return nil
}
