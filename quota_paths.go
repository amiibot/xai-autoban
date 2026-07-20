package main

import (
	"os"
	"path/filepath"
	"strings"
)

// pathRoots returns candidate CPA install roots without hard-coded drive letters.
// Order: explicit env → process cwd → parents → relative layouts.
func pathRoots() []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		key := strings.ToLower(p)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}

	for _, env := range []string{"CPA_ROOT", "CLIPROXY_ROOT", "CLIPROXYAPI_ROOT", "XAI_AUTOBAN_CPA_ROOT", "GROK_QUOTA_CPA_ROOT"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			add(v)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		add(cwd)
		add(filepath.Join(cwd, "CPA"))
		add(filepath.Dir(cwd))
		add(filepath.Join(filepath.Dir(cwd), "CPA"))
		// plugin often runs with cwd = CPA root or plugins/<os>/<arch>
		add(filepath.Clean(filepath.Join(cwd, "..")))
		add(filepath.Clean(filepath.Join(cwd, "..", "..")))
		add(filepath.Clean(filepath.Join(cwd, "..", "..", "..")))
	}
	add(".")
	return out
}

func firstExistingDir(candidates ...string) string {
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return ""
}

func firstExistingFile(candidates ...string) string {
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

func underRoots(parts ...string) []string {
	var out []string
	for _, root := range pathRoots() {
		out = append(out, filepath.Join(append([]string{root}, parts...)...))
	}
	out = append(out, filepath.Join(parts...))
	return out
}

func detectUsageDBPath(explicit string) string {
	candidates := []string{}
	if v := strings.TrimSpace(explicit); v != "" {
		candidates = append(candidates, v)
	}
	for _, env := range []string{
		"XAI_AUTOBAN_USAGE_DB",
		"GROK_QUOTA_CPAMP_DB",
		"CPAMP_USAGE_DB",
		"GROK_PANEL_CPAMP_DB",
	} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			candidates = append(candidates, v)
		}
	}
	for _, root := range pathRoots() {
		candidates = append(candidates,
			filepath.Join(root, "CPAMP", "data", "usage.sqlite"),
			filepath.Join(filepath.Dir(root), "CPAMP", "data", "usage.sqlite"),
			filepath.Join(root, "data", "usage.sqlite"),
		)
	}
	candidates = append(candidates,
		filepath.Join("CPAMP", "data", "usage.sqlite"),
		filepath.Join("data", "usage.sqlite"),
	)
	return firstExistingFile(candidates...)
}

func detectAuthDir(explicit string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	for _, env := range []string{"XAI_AUTOBAN_AUTH_DIR", "GROK_QUOTA_AUTH_DIR", "CPA_AUTH_DIR"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	if found := firstExistingDir(underRoots("auths")...); found != "" {
		return found
	}
	return filepath.Join("auths")
}
