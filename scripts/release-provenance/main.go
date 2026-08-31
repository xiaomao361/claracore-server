package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"claracore-server/internal/buildprovenance"
)

func main() {
	mode := flag.String("mode", "", "generate or validate")
	repo := flag.String("repo", "", "absolute repository directory (generate only)")
	stage := flag.String("stage", "", "absolute release directory")
	binary := flag.String("binary", "", "absolute release binary path")
	provenance := flag.String("provenance", "", "absolute provenance path override (validate only)")
	version := flag.String("version", "", "release version")
	sourceDigest := flag.String("source-digest", "", "linked application source digest")
	arch := flag.String("arch", "", "target Linux architecture")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal("positional arguments are not accepted")
	}
	switch *mode {
	case "snapshot-inputs":
		if *binary != "" || *provenance != "" || *version != "" || *sourceDigest != "" || *arch != "" {
			fatal("snapshot-inputs accepts only -repo and -stage")
		}
		if err := buildprovenance.CaptureInputs(*repo, *stage); err != nil {
			fatal(err.Error())
		}
		fmt.Printf("build_inputs=snapshotted manifest=%s\n", filepath.Join(*stage, buildprovenance.BuildInputsName))
	case "generate":
		if *provenance != "" {
			fatal("-provenance is validate-only")
		}
		if err := buildprovenance.Generate(buildprovenance.GenerateInput{
			RepoDir: *repo, ReleaseDir: *stage, BinaryPath: *binary,
			Version: *version, SourceDigest: *sourceDigest, TargetOS: "linux", TargetArch: *arch,
		}); err != nil {
			fatal(err.Error())
		}
		fmt.Printf("provenance=generated statement=%s predicate=%s builder=%s\n",
			filepath.Join(*stage, buildprovenance.FileName), buildprovenance.PredicateType, buildprovenance.BuilderID)
	case "validate":
		if *repo != "" {
			fatal("-repo is generate-only")
		}
		result, err := buildprovenance.Verify(buildprovenance.VerifyInput{
			ReleaseDir: *stage, BinaryPath: *binary, ProvenancePath: *provenance,
			Version: *version, SourceDigest: *sourceDigest,
			TargetOS: "linux", TargetArch: *arch, GoVersion: buildprovenance.ExpectedGoVersion,
		})
		if err != nil {
			fatal(err.Error())
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fatal(err.Error())
		}
	default:
		fatal("-mode must be snapshot-inputs, generate or validate")
	}
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "release provenance:", message)
	os.Exit(1)
}
