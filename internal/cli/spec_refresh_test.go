package cli

import (
	"strings"
	"testing"
)

func TestRefreshedHelpHasNoHTTPParameterProse(t *testing.T) {
	app, output, _ := testApp(t, "http://127.0.0.1:1")
	for _, command := range app.Registry.Commands {
		output.Reset()
		if exit := app.showCommandHelp(command, defaultOptions()); exit != ExitOK {
			t.Fatalf("%s help exited %d", command.Name, exit)
		}
		for _, obsolete := range []string{"%2B", "Repeat the parameter"} {
			if strings.Contains(output.String(), obsolete) {
				t.Errorf("%s help retains HTTP parameter prose %q", command.Name, obsolete)
			}
		}
	}
}
