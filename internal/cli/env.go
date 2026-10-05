package cli

import (
	"fmt"
	"net/http"
	"time"
)

func (a *app) envVars(args []string) error {
	if len(args) == 0 {
		return usagef("missing subcommand (set, rm or ls)")
	}
	sub, args := args[0], args[1:]
	fs := a.flags("env")
	switch sub {
	case "set":
		pos, err := a.parse(fs, args, 3, 3)
		if err != nil {
			return err
		}
		resp, err := a.client().call(a.ctx, http.MethodPut, "/api/flats/"+pathEscape(pos[0])+"/env/"+pathEscape(pos[1]), nil, map[string]string{"value": pos[2]}, nil)
		if err != nil {
			return err
		}
		if a.jsonOut {
			a.emitRaw(resp)
			return nil
		}
		fmt.Fprintf(a.out, "Stored %s for %s. Changes apply to live on the next deploy/redeploy, rollback or data restoration (after any required approval), or Flats host restart. New previews capture current settings; automatic worker restarts reuse their captured settings.\n", pos[1], pos[0])
		return nil
	case "rm", "delete":
		pos, err := a.parse(fs, args, 2, 2)
		if err != nil {
			return err
		}
		resp, err := a.client().call(a.ctx, http.MethodDelete, "/api/flats/"+pathEscape(pos[0])+"/env/"+pathEscape(pos[1]), nil, nil, nil)
		if err != nil {
			return err
		}
		if a.jsonOut {
			a.emitRaw(resp)
			return nil
		}
		fmt.Fprintf(a.out, "Deleted %s from %s. Changes apply to live on the next deploy/redeploy, rollback or data restoration (after any required approval), or Flats host restart. New previews capture current settings; automatic worker restarts reuse their captured settings.\n", pos[1], pos[0])
		return nil
	case "ls", "list":
		pos, err := a.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		var out struct {
			Env []struct {
				Name      string    `json:"name"`
				Value     string    `json:"value"`
				UpdatedAt time.Time `json:"updated_at"`
			} `json:"env"`
		}
		resp, err := a.client().call(a.ctx, http.MethodGet, "/api/flats/"+pathEscape(pos[0])+"/env", nil, nil, &out)
		if err != nil {
			return err
		}
		if a.jsonOut {
			a.emitRaw(resp)
			return nil
		}
		if len(out.Env) == 0 {
			fmt.Fprintln(a.out, "No environment variables.")
			return nil
		}
		for _, v := range out.Env {
			fmt.Fprintf(a.out, "%s=%q\n", v.Name, v.Value)
		}
		return nil
	}
	return usagef("unknown env subcommand %q (use set, rm or ls)", sub)
}
