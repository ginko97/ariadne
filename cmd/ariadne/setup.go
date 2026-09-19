package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
)

// configEnvFile is config.env in the home directory. main sets it alongside
// runsDir; the value here is what the tests see.
var configEnvFile = "config.env"

// providers are the endpoints setup knows by name. Anything else is "other",
// which asks for a URL and stores its key as ARIADNE_API_KEY — the only key
// an unrecognised host is ever given (see apiKey).
var providers = map[string]string{
	"openrouter": "https://openrouter.ai/api/v1",
	"openai":     "https://api.openai.com/v1",
	"gemini":     "https://generativelanguage.googleapis.com/v1beta/openai",
	"xai":        "https://api.x.ai/v1",
	// Ollama needs no key; setup asks it which models are pulled instead.
	"ollama": ollamaURL,
}

const ollamaURL = "http://localhost:11434/v1"

// cmdSetup writes config.env so an installed binary works with no flags: the
// endpoint, the key for it, and optionally a model. It then makes one tiny
// request to prove the key and model work, because a setup that reports
// success and then fails on the first real question is worse than none.
func cmdSetup(args []string) int {
	return runSetup(os.Stdin, os.Stderr, args)
}

func runSetup(stdin io.Reader, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(out)
	provider := fs.String("provider", "", "openrouter (default), openai, gemini, xai, ollama, or other")
	baseURL := fs.String("base-url", "", "endpoint, for -provider other (or Ollama somewhere other than "+ollamaURL+")")
	model := fs.String("model", "", "model to use by default (default: one chosen for the provider)")
	noCheck := fs.Bool("no-check", false, "write the config without testing the key")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	in := bufio.NewReader(stdin)
	ask := func(prompt string) string {
		fmt.Fprint(out, prompt)
		line, _ := in.ReadString('\n')
		return strings.TrimSpace(line)
	}

	if *provider == "" {
		names := make([]string, 0, len(providers))
		for n := range providers {
			names = append(names, n)
		}
		sort.Strings(names)
		*provider = ask(fmt.Sprintf("provider (%s, other) [openrouter]: ", strings.Join(names, ", ")))
		if *provider == "" {
			*provider = "openrouter"
		}
	}
	*provider = strings.ToLower(*provider)

	url, known := providers[*provider]
	switch {
	case *provider == "ollama":
		if *baseURL != "" {
			url = *baseURL
		}
		return setupOllama(out, ask, url, *model, *noCheck)
	case known:
	case *provider == "other":
		url = *baseURL
		if url == "" {
			url = ask("endpoint URL (OpenAI-compatible, e.g. http://localhost:11434/v1): ")
		}
		if url == "" {
			fmt.Fprintln(out, "ariadne setup: -provider other needs an endpoint URL")
			return exitUsage
		}
	default:
		fmt.Fprintf(out, "ariadne setup: unknown provider %q\n", *provider)
		return exitUsage
	}

	keyName := providerKeyName(url)
	if keyName == "" {
		keyName = "ARIADNE_API_KEY"
	}
	key := readSecret(stdin, in, out, fmt.Sprintf("%s (input hidden): ", keyName))
	if key == "" {
		fmt.Fprintln(out, "ariadne setup: no key entered; nothing written")
		return exitUsage
	}

	if *model == "" {
		*model = defaultModelFor(url)
	}

	if !*noCheck {
		fmt.Fprintf(out, "checking %s with %s ... ", url, *model)
		if err := checkKey(context.Background(), url, key, *model); err != nil {
			fmt.Fprintf(out, "failed\nariadne setup: %v\nnothing written; fix the key or model, or rerun with -no-check\n", err)
			return exitFail
		}
		fmt.Fprintln(out, "ok")
	}

	vals := configVals(url, key, *model)
	if err := writeConfigEnv(configEnvFile, vals); err != nil {
		fmt.Fprintf(out, "ariadne setup: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(out, "wrote %s\n", configEnvFile)

	// The process environment beats config.env, so a key already exported in
	// the shell would silently win over the one just typed.
	for _, name := range []string{keyName, "ARIADNE_BASE_URL", "ARIADNE_MODEL"} {
		if v, ok := os.LookupEnv(name); ok && v != vals[name] {
			fmt.Fprintf(out, "note: %s is also set in your environment and takes precedence over config.env\n", name)
		}
	}
	if keyName != "ARIADNE_API_KEY" && os.Getenv("ARIADNE_API_KEY") != "" {
		fmt.Fprintln(out, "note: ARIADNE_API_KEY is set in your environment and is used for every endpoint")
	}

	fmt.Fprint(out, "\nnext:\n  ariadne ui      talk in the browser\n  ariadne chat    talk in the terminal\n")
	return exitOK
}

// setupOllama configures a local Ollama. There is no key to ask for; what can
// go wrong is that Ollama is not running or has no model pulled, and both are
// found by asking it before anything is written. No key is stored either:
// apiKey gives a loopback endpoint a placeholder of its own.
func setupOllama(out io.Writer, ask func(string) string, url, model string, noCheck bool) int {
	if noCheck && model == "" {
		fmt.Fprintln(out, "ariadne setup: -no-check with -provider ollama needs -model")
		return exitUsage
	}
	if !noCheck {
		models, err := ollamaModels(context.Background(), url)
		if err != nil {
			fmt.Fprintf(out, "ariadne setup: Ollama is not answering at %s: %v\n"+
				"start it (open the Ollama app, or run `ollama serve`), then run setup again\n", ollamaRoot(url), err)
			return exitFail
		}
		if len(models) == 0 {
			fmt.Fprint(out, "ariadne setup: Ollama is running but has no models; pull one that can call tools, e.g.\n"+
				"  ollama pull qwen3\nthen run setup again\n")
			return exitFail
		}
		if model == "" {
			fmt.Fprintf(out, "models in Ollama: %s\n", strings.Join(models, ", "))
			if model = ask(fmt.Sprintf("model [%s]: ", models[0])); model == "" {
				model = models[0]
			}
		}
		fmt.Fprintf(out, "checking %s with %s ... ", url, model)
		if err := checkKey(context.Background(), url, localNoKey, model); err != nil {
			fmt.Fprintf(out, "failed\nariadne setup: %v\nnothing written; ariadne needs a model that can call tools\n", err)
			return exitFail
		}
		fmt.Fprintln(out, "ok")
	}

	// ARIADNE_API_KEY is cleared for the same reason as for a named provider:
	// it goes to every endpoint, so a key left from an earlier setup would be
	// sent here, and would outrank the provider keys once -base-url points
	// elsewhere.
	ollamaKey := ""
	if !isLoopbackURL(url) {
		// Ollama on another machine: apiKey gives only loopback a placeholder,
		// so this one is stored. It is not a secret, and it goes to every
		// endpoint, which is why the loopback case does not store it.
		ollamaKey = localNoKey
		fmt.Fprintf(out, "note: %s is not this machine, so ARIADNE_API_KEY=%s is stored for it;\n"+
			"ARIADNE_API_KEY outranks every provider's key, so rerun setup before using another provider\n", url, localNoKey)
	}
	vals := configVals(url, ollamaKey, model)
	if err := writeConfigEnv(configEnvFile, vals); err != nil {
		fmt.Fprintf(out, "ariadne setup: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(out, "wrote %s\n", configEnvFile)
	for _, name := range []string{"ARIADNE_BASE_URL", "ARIADNE_MODEL"} {
		if v, ok := os.LookupEnv(name); ok && v != vals[name] {
			fmt.Fprintf(out, "note: %s is also set in your environment and takes precedence over config.env\n", name)
		}
	}
	if os.Getenv("ARIADNE_API_KEY") != "" {
		fmt.Fprintln(out, "note: ARIADNE_API_KEY is set in your environment and is sent to every endpoint, Ollama included")
	}
	fmt.Fprint(out, "\nnext:\n  ariadne ui      talk in the browser\n  ariadne chat    talk in the terminal\n")
	return exitOK
}

// configVals is what a configuration writes to config.env: the endpoint, the
// model, and the key under the one name apiKey will send only to that
// endpoint's host.
//
// ARIADNE_API_KEY goes to every endpoint and outranks every provider's key, so
// it is written only when the key has no other name, and cleared otherwise —
// a value left from an earlier setup would be sent to the new provider in
// place of its own key. An empty value is how writeConfigEnv removes a line.
// A loopback endpoint with no key stores none: apiKey supplies a placeholder.
func configVals(url, key, model string) map[string]string {
	vals := map[string]string{"ARIADNE_BASE_URL": url, "ARIADNE_MODEL": model, "ARIADNE_API_KEY": ""}
	switch name := providerKeyName(url); {
	case name != "":
		vals[name] = key
	case isLoopbackURL(url) && (key == "" || key == localNoKey):
	default:
		vals["ARIADNE_API_KEY"] = key
	}
	return vals
}

// ollamaRoot is the server behind an Ollama OpenAI-compatible URL: /v1 is
// where conversations go, the native /api is where the pulled models are listed.
func ollamaRoot(url string) string {
	return strings.TrimSuffix(strings.TrimSuffix(url, "/"), "/v1")
}

// ollamaModels asks a running Ollama which models are pulled.
func ollamaModels(ctx context.Context, url string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaRoot(url)+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/tags: %s", resp.Status)
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tags); err != nil {
		return nil, fmt.Errorf("GET /api/tags: %v", err)
	}
	var names []string
	for _, m := range tags.Models {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	return names, nil
}

// checkKey makes the smallest real request that looks like a conversation: one
// short message and one tool on offer. The tool is there because every
// conversation offers tools, and a model that cannot take them (common among
// local models) is refused with a 400 — better here, before anything is
// written, than on the first real question.
func checkKey(ctx context.Context, url, key, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	client := llm.NewOpenAI(key, llm.WithBaseURL(url), llm.WithTimeout(60*time.Second), llm.WithMaxRetries(0))
	_, err := client.Complete(ctx, llm.Request{
		Model: model,
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "Reply with the single word: ready"},
		}}},
		Tools: []llm.ToolDef{{
			Name:        "ready",
			Description: "Not needed; answer in text.",
			Schema:      []byte(`{"type":"object","properties":{}}`),
		}},
	})
	return err
}

// readSecret reads one line without echoing it when stdin is a terminal, and
// plainly otherwise, so `echo $KEY | ariadne setup` works too. in is the
// buffered reader over stdin already in use, so no input is lost between them.
func readSecret(stdin io.Reader, in *bufio.Reader, out io.Writer, prompt string) string {
	fmt.Fprint(out, prompt)
	f, isFile := stdin.(*os.File)
	if isFile && isTerminal(f) {
		if restore, err := echoOff(f); err == nil {
			defer func() {
				restore()
				fmt.Fprintln(out)
			}()
		}
	}
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

// writeConfigEnv sets vals in the KEY=value file at path, keeping every other
// line, and writes it readable by its owner only. An empty value is not
// written. Atomic: a crash mid-write leaves the old file, not half a new one.
func writeConfigEnv(path string, vals map[string]string) error {
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		s := strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		if s != "" {
			lines = strings.Split(s, "\n")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	done := map[string]bool{}
	var kept []string
	for _, l := range lines {
		k, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(l), "export "), "=")
		k = strings.TrimSpace(k)
		if v, set := vals[k]; ok && set {
			if v != "" {
				kept = append(kept, k+"="+v)
			}
			done[k] = true
		} else {
			kept = append(kept, l)
		}
	}
	lines = kept
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if !done[k] && vals[k] != "" {
			lines = append(lines, k+"="+vals[k])
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.env")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	// CreateTemp already makes the file 0600 on Unix. Windows ignores most
	// mode bits; the file lives in the user's profile, which is the
	// protection there.
	_ = tmp.Chmod(0o600)
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
