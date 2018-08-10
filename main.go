package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bitrise-io/go-utils/command"
	"github.com/bitrise-io/go-utils/command/rubycommand"
	"github.com/bitrise-io/go-utils/fileutil"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-utils/retry"
	"github.com/bitrise-tools/go-steputils/input"
	"github.com/kballard/go-shellquote"
)

// ConfigsModel ...
type ConfigsModel struct {
	GitURL          string
	GitBranch       string
	AppID           string
	DecryptPassword string
	Type            string
	TeamID          string

	Options         string
	GemfilePath     string
	FastlaneVersion string
}

func createConfigsModelFromEnvs() ConfigsModel {
	return ConfigsModel{
		GitURL:          os.Getenv("git_url"),
		GitBranch:       os.Getenv("git_branch"),
		AppID:           os.Getenv("app_id"),
		DecryptPassword: os.Getenv("decrypt_password"),
		Type:            os.Getenv("type"),
		TeamID:          os.Getenv("team_id"),

		Options:         os.Getenv("options"),
		GemfilePath:     os.Getenv("gemfile_path"),
		FastlaneVersion: os.Getenv("fastlane_version"),
	}
}

func (configs ConfigsModel) print() {
	log.Infof("Configs:")

	log.Printf("- GitURL: %s", configs.GitURL)
	log.Printf("- GitBranch: %s", configs.GitBranch)
	log.Printf("- AppID: %s", configs.AppID)
	log.Printf("- DecryptPassword: %s", input.SecureInput(configs.DecryptPassword))
	log.Printf("- Type: %s", configs.Type)
	log.Printf("- TeamID: %s", configs.TeamID)

	log.Printf("- Options: %s", configs.Options)
	log.Printf("- GemfilePath: %s", configs.GemfilePath)
	log.Printf("- FastlaneVersion: %s", configs.FastlaneVersion)
}

func (configs ConfigsModel) validate() error {
	if err := input.ValidateIfNotEmpty(configs.GitURL); err != nil {
		return fmt.Errorf("Git Url %s", err)
	}

	if err := input.ValidateIfNotEmpty(configs.AppID); err != nil {
		return fmt.Errorf("App ID %s", err)
	}

	if err := input.ValidateIfNotEmpty(configs.DecryptPassword); err != nil {
		return fmt.Errorf("Decrypt Password %s", err)
	}

	if err := input.ValidateWithOptions(configs.Type, "adhoc", "appstore", "development", "enterprise"); err != nil {
		return fmt.Errorf("Type, %s", err)
	}

	return nil
}

func fail(format string, v ...interface{}) {
	log.Errorf(format, v...)
	os.Exit(1)
}

func gemInstallWithRetry(gemName string, version string) error {
	return retry.Times(2).Try(func(attempt uint) error {
		if attempt > 0 {
			log.Warnf("%d attempt failed", attempt+1)
		}

		versionToInstall := version

		if versionToInstall == "latest" {
			versionToInstall = ""
		}

		cmds, err := rubycommand.GemInstall(gemName, versionToInstall)
		if err != nil {
			return fmt.Errorf("Failed to create command, error: %s", err)
		}

		for _, cmd := range cmds {
			if out, err := cmd.RunAndReturnTrimmedCombinedOutput(); err != nil {
				return fmt.Errorf("Gem install failed, output: %s, error: %s", out, err)
			}
		}

		return nil
	})
}

func gemVersionFromGemfileLockContent(gem, content string) string {
	relevantLines := []string{}
	lines := strings.Split(content, "\n")

	specsStart := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}

		if trimmed == "specs:" {
			specsStart = true
			continue
		}

		if specsStart {
			relevantLines = append(relevantLines, trimmed)
		}
	}

	exp := regexp.MustCompile(fmt.Sprintf(`^%s \((.+)\)`, gem))
	for _, line := range relevantLines {
		match := exp.FindStringSubmatch(line)
		if match != nil && len(match) == 2 {
			return match[1]
		}
	}

	return ""
}

func gemVersionFromGemfileLock(gem, gemfileLockPth string) (string, error) {
	content, err := fileutil.ReadStringFromFile(gemfileLockPth)
	if err != nil {
		return "", err
	}
	return gemVersionFromGemfileLockContent(gem, content), nil
}

func ensureFastlaneVersionAndCreateCmdSlice(forceVersion, gemfilePth string) ([]string, string, error) {
	if forceVersion != "" {
		log.Printf("fastlane version defined: %s, installing...", forceVersion)

		newVersion := forceVersion
		if forceVersion == "latest" {
			newVersion = ""
		}

		if err := gemInstallWithRetry("fastlane", newVersion); err != nil {
			return nil, "", err
		}

		fastlaneCmdSlice := []string{"fastlane"}
		if newVersion != "" {
			fastlaneCmdSlice = append(fastlaneCmdSlice, fmt.Sprintf("_%s_", newVersion))
		}

		return fastlaneCmdSlice, "", nil
	}

	if gemfilePth == "" {
		log.Printf("no fastlane version nor Gemfile path defined, using system installed fastlane...")
		return []string{"fastlane"}, "", nil
	}

	if exist, err := pathutil.IsPathExists(gemfilePth); err != nil {
		return nil, "", err
	} else if !exist {
		log.Printf("Gemfile not exist at: %s and no fastlane version defined, using system installed fastlane...", gemfilePth)
		return []string{"fastlane"}, "", nil
	}

	log.Printf("Gemfile exist, checking fastlane version from Gemfile.lock")

	gemfileDir := filepath.Dir(gemfilePth)
	gemfileLockPth := filepath.Join(gemfileDir, "Gemfile.lock")

	bundleInstallCalled := false
	if exist, err := pathutil.IsPathExists(gemfileLockPth); err != nil {
		return nil, "", err
	} else if !exist {
		log.Printf("Gemfile.lock not exist at: %s, running 'bundle install' ...", gemfileLockPth)

		cmd := command.NewWithStandardOuts("bundle", "install").SetStdin(os.Stdin).SetDir(gemfileDir)
		if err := cmd.Run(); err != nil {
			return nil, "", err
		}

		bundleInstallCalled = true

		if exist, err := pathutil.IsPathExists(gemfileLockPth); err != nil {
			return nil, "", err
		} else if !exist {
			return nil, "", errors.New("Gemfile.lock does not exist, even 'bundle install' was called")
		}
	}

	fastlaneVersion, err := gemVersionFromGemfileLock("fastlane", gemfileLockPth)
	if err != nil {
		return nil, "", err
	}

	if fastlaneVersion != "" {
		log.Printf("fastlane version defined in Gemfile.lock: %s, using bundler to call fastlane commands...", fastlaneVersion)

		if !bundleInstallCalled {
			cmd := command.NewWithStandardOuts("bundle", "install").SetStdin(os.Stdin).SetDir(gemfileDir)
			if err := cmd.Run(); err != nil {
				return nil, "", err
			}
		}

		return []string{"bundle", "exec", "fastlane"}, gemfileDir, nil
	}

	log.Printf("fastlane version not found in Gemfile.lock, using system installed fastlane...")

	return []string{"fastlane"}, "", nil
}

func main() {
	configs := createConfigsModelFromEnvs()

	fmt.Println()
	configs.print()

	if err := configs.validate(); err != nil {
		fail("Issue with input: %s", err)
	}

	//
	// Setup
	fmt.Println()
	log.Infof("Setup")

	startTime := time.Now()

	fastlaneCmdSlice, workDir, err := ensureFastlaneVersionAndCreateCmdSlice(configs.FastlaneVersion, configs.GemfilePath)
	if err != nil {
		fail("Failed to ensure fastlane version, error: %s", err)
	}

	versionCmdSlice := append(fastlaneCmdSlice, "-v")
	versionCmd := command.NewWithStandardOuts(versionCmdSlice[0], versionCmdSlice[1:]...)
	log.Printf("$ %s", versionCmd.PrintableCommandArgs())
	if err := versionCmd.Run(); err != nil {
		fail("Failed to print fastlane version, error: %s", err)
	}

	elapsed := time.Since(startTime)

	log.Printf("Setup took %f seconds to complete", elapsed.Seconds())

	//
	// Main
	fmt.Println()
	log.Infof("Running Match")

	fmt.Println()

	options := []string{}
	if configs.Options != "" {
		opts, err := shellquote.Split(configs.Options)
		if err != nil {
			fail("Failed to split options (%s), error: %s", configs.Options, err)
		}
		options = opts
	}

	envs := []string{
		fmt.Sprintf("MATCH_PASSWORD=%s", configs.DecryptPassword),
	}

	args := []string{
		"match",
		configs.Type,
		"--readonly",
	}

	args = append(args, "--git_url", configs.GitURL)
	args = append(args, "--app_identifier", configs.AppID)

	if configs.GitBranch != "" {
		args = append(args, "--git_branch", configs.GitBranch)
	}
	
	if configs.TeamID != "" {
		args = append(args, "--team_id", configs.TeamID)
	}	

	args = append(args, options...)

	cmdSlice := append(fastlaneCmdSlice, args...)

	cmd := command.New(cmdSlice[0], cmdSlice[1:]...)
	log.Donef("$ %s", cmd.PrintableCommandArgs())

	cmd.SetStdout(os.Stdout)
	cmd.SetStderr(os.Stderr)
	cmd.SetStdin(os.Stdin)
	cmd.AppendEnvs(envs...)
	if workDir != "" {
		cmd.SetDir(workDir)
	}

	fmt.Println()

	if err := cmd.Run(); err != nil {
		fail("Download or installation failed, error: %s", err)
	}

	log.Donef("Success")
}
