package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// The launcher's command line (muster #439):
//
//	muster __sandbox-exec (--policy-file PATH | --policy JSON)
//	    [--self-test OUTSIDE_FILE TCP_PORT] -- /absolute/program [args...]
//
// It is parsed here, with no build tag, so the rules about what the launcher
// refuses are tested on every platform. Nothing is read from the environment: the
// policy is data the service wrote, and a session's own environment is exactly
// what must not be able to change it.

// maxPolicyBytes bounds a policy file. A real policy is a few kilobytes of paths.
const maxPolicyBytes = 1 << 20

type launcherArgs struct {
	policy LinuxPolicy
	// selfTest runs the in-process checks instead of executing a program.
	selfTest    bool
	outsideFile string
	tcpPort     int
	// argv is the program and its arguments; argv[0] is its absolute path.
	argv []string
}

func parseLauncherArgs(args []string) (launcherArgs, error) {
	var (
		out        launcherArgs
		policyFile string
		policyText string
		haveFile   bool
		haveText   bool
	)
	i := 0
loop:
	for i < len(args) {
		switch args[i] {
		case "--":
			i++
			break loop
		case "--policy-file":
			if i+1 >= len(args) {
				return out, errors.New("--policy-file needs a path")
			}
			policyFile, haveFile = args[i+1], true
			i += 2
		case "--policy":
			if i+1 >= len(args) {
				return out, errors.New("--policy needs a document")
			}
			policyText, haveText = args[i+1], true
			i += 2
		case "--self-test":
			if i+2 >= len(args) {
				return out, errors.New("--self-test needs a file outside the grants and a TCP port")
			}
			port, err := strconv.Atoi(args[i+2])
			if err != nil || port < 1 || port > 65535 {
				return out, fmt.Errorf("--self-test port %q is not a port", args[i+2])
			}
			out.selfTest, out.outsideFile, out.tcpPort = true, args[i+1], port
			i += 3
		default:
			return out, fmt.Errorf("unknown argument %q", args[i])
		}
	}
	rest := args[i:]

	switch {
	case haveFile && haveText:
		return out, errors.New("give the policy as a file or as an argument, not both")
	case !haveFile && !haveText:
		return out, errors.New("no policy was given")
	}
	var raw []byte
	if haveFile {
		b, err := readPolicyFile(policyFile)
		if err != nil {
			return out, err
		}
		raw = b
	} else {
		raw = []byte(policyText)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out.policy); err != nil {
		return out, fmt.Errorf("the policy does not parse: %w", err)
	}
	if err := out.policy.Validate(); err != nil {
		return out, err
	}

	if out.selfTest {
		out.argv = rest
		return out, nil
	}
	if len(rest) == 0 {
		return out, errors.New("no program to run")
	}
	// No PATH lookup: a resolution the session's environment could influence is
	// not one the policy was built for.
	if !filepath.IsAbs(rest[0]) {
		return out, fmt.Errorf("the program %q is not an absolute path", rest[0])
	}
	out.argv = rest
	return out, nil
}

func readPolicyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the policy: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPolicyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the policy: %w", err)
	}
	if len(b) > maxPolicyBytes {
		return nil, fmt.Errorf("the policy is larger than %d bytes", maxPolicyBytes)
	}
	return b, nil
}
