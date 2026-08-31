package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"claracore-server/internal/buildrepro"
)

func main() {
	mode := flag.String("mode", "", "generate or validate")
	stage := flag.String("stage", "", "absolute release directory")
	primary := flag.String("primary", "", "absolute primary release binary")
	rebuilt := flag.String("rebuilt", "", "absolute independently rebuilt binary (generate only)")
	receipt := flag.String("receipt", "", "absolute receipt path override (validate only)")
	version := flag.String("version", "", "release version")
	sourceDigest := flag.String("source-digest", "", "linked application source digest")
	arch := flag.String("arch", "", "target Linux architecture")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal("positional arguments are not accepted")
	}
	identity := buildrepro.Identity{
		Version: *version, SourceDigest: *sourceDigest, TargetOS: "linux",
		TargetArch: *arch, GoToolchain: "go1.26.6",
	}
	switch *mode {
	case "generate":
		if *receipt != "" {
			fatal("-receipt is validate-only")
		}
		if err := buildrepro.Generate(buildrepro.GenerateInput{
			ReleaseDir: *stage, PrimaryBinary: *primary, RebuiltBinary: *rebuilt, Identity: identity,
		}); err != nil {
			fatal(err.Error())
		}
		fmt.Printf("reproducibility=matched scope=%s\n", buildrepro.Scope)
	case "validate":
		if *rebuilt != "" {
			fatal("-rebuilt is generate-only")
		}
		result, err := buildrepro.Verify(buildrepro.VerifyInput{
			ReleaseDir: *stage, PrimaryBinary: *primary, ReceiptPath: *receipt, Identity: identity,
		})
		if err != nil {
			fatal(err.Error())
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fatal(err.Error())
		}
	default:
		fatal("-mode must be generate or validate")
	}
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "release reproducibility:", message)
	os.Exit(1)
}
