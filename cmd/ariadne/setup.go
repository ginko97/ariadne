package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
}

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
	provider := fs.String("provider", "", "openrouter (default), openai, gemini, xai, or other")
	baseURL := fs.String("base-url", "", "endpoint, for -provider other")
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
		if err := checkKey(url, key, *model); err != nil {
			fmt.Fprintf(out, "failed\nariadne setup: %v\nnothing written; fix the key or model, or rerun with -no-check\n", err)
			return exitFail
		}
		fmt.Fprintln(out, "ok")
	}

	vals := map[string]string{"ARIADNE_BASE_URL": url, keyName: key, "ARIADNE_MODEL": *model}
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

// checkKey makes the smallest real request: one short message, no tools.
func checkKey(url, key, model string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := llm.NewOpenAI(key, llm.WithBaseURL(url), llm.WithTimeout(60*time.Second), llm.WithMaxRetries(0))
	_, err := client.Complete(ctx, llm.Request{
		Model: model,
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "Reply with the single word: ready"},
		}}},
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
		lines = strings.Split(strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	done := map[string]bool{}
	for i, l := range lines {
		k, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(l), "export "), "=")
		k = strings.TrimSpace(k)
		if v, set := vals[k]; ok && set {
			if v == "" {
				lines[i] = ""
			} else {
				lines[i] = k + "=" + v
			}
			done[k] = true
		}
	}
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
