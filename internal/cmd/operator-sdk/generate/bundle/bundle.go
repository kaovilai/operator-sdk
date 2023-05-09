// Copyright 2020 The Operator-SDK Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bundle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/operator-framework/api/pkg/apis/scorecard/v1alpha3"
	"github.com/operator-framework/operator-manifest-tools/pkg/image"
	"github.com/operator-framework/operator-manifest-tools/pkg/imageresolver"
	"github.com/operator-framework/operator-manifest-tools/pkg/pullspec"
	"github.com/operator-framework/operator-registry/pkg/lib/bundle"
	metricsannotations "github.com/operator-framework/operator-sdk/internal/annotations/metrics"
	genutil "github.com/operator-framework/operator-sdk/internal/cmd/operator-sdk/generate/internal"
	gencsv "github.com/operator-framework/operator-sdk/internal/generate/clusterserviceversion"
	"github.com/operator-framework/operator-sdk/internal/generate/clusterserviceversion/bases"
	"github.com/operator-framework/operator-sdk/internal/generate/collector"
	"github.com/operator-framework/operator-sdk/internal/registry"
	"github.com/operator-framework/operator-sdk/internal/scorecard"
	"github.com/operator-framework/operator-sdk/internal/util/bundleutil"
)

const (
	longHelp = `
Running 'generate bundle' is the first step to publishing your operator to a catalog and deploying it with OLM.
This command both generates and packages files into an on-disk representation of an operator called a bundle.
A bundle consists of a ClusterServiceVersion (CSV), CustomResourceDefinitions (CRDs),
manifests not part of the CSV but required by the operator, some metadata (annotations.yaml),
and a bundle.Dockerfile to build a bundle image.

A CSV manifest is generated by collecting data from the set of manifests passed to this command (see below),
such as CRDs, RBAC, etc., and applying that data to a "base" CSV manifest. This base CSV can contain metadata,
added by hand or by the 'generate kustomize manifests' command, and can be passed in like any other manifest
(see below) or by file at the exact path '<kustomize-dir>/bases/<package-name>.clusterserviceversion.yaml'.
Be aware that 'generate bundle' idempotently regenerates a bundle, so all non-metadata values in a base
will be overwritten. If no base was passed in, input manifest data will be applied to an empty CSV.

There are two ways to pass the to-be-bundled set of manifests to this command: stdin via a Unix pipe,
or in a directory using '--input-dir'. See command help for more information on these modes.
Passing a directory is useful for running 'generate bundle' outside of a project or within a project
that does not use kustomize and/or contains cluster-ready manifests on disk.

Set '--version' to supply a semantic version for your bundle if you are creating one
for the first time or upgrading an existing one.

If '--output-dir' is set and you wish to build bundle images from that directory,
either manually update your bundle.Dockerfile or set '--overwrite'.

More information on bundles:
https://github.com/operator-framework/operator-registry/#manifest-format
`

	examples = `
  # If running within a project or in a project that uses kustomize to generate manifests,
	# make sure a kustomize directory exists that looks like the following 'config/manifests' directory:
  $ tree config/manifests
  config/manifests
  ├── bases
  │   └── memcached-operator.clusterserviceversion.yaml
  └── kustomization.yaml

  # Generate a 0.0.1 bundle by passing manifests to stdin:
  $ kustomize build config/manifests | operator-sdk generate bundle --version 0.0.1
  Generating bundle version 0.0.1
  ...

  # If running outside of a project or in a project that does not use kustomize to generate manifests,
	# make sure cluster-ready manifests are available on disk:
  $ tree deploy/
  deploy/
  ├── crds
  │   └── cache.my.domain_memcacheds.yaml
  ├── deployment.yaml
  ├── role.yaml
  ├── role_binding.yaml
  ├── service_account.yaml
  └── webhooks.yaml

  # Generate a 0.0.1 bundle by passing manifests by dir:
  $ operator-sdk generate bundle --input-dir deploy --version 0.0.1
  Generating bundle version 0.0.1
  ...

  # After running in either of the above modes, you should see this directory structure:
  $ tree bundle/
  bundle/
  ├── manifests
  │   ├── cache.my.domain_memcacheds.yaml
  │   └── memcached-operator.clusterserviceversion.yaml
  └── metadata
      └── annotations.yaml
`
)

// defaultRootDir is the default root directory in which to generate bundle files.
const defaultRootDir = "bundle"

// setDefaults sets defaults useful to all modes of this subcommand.
func (c *bundleCmd) setDefaults() (err error) {
	if c.packageName, c.layout, err = genutil.GetPackageNameAndLayout(c.packageName); err != nil {
		return err
	}
	return nil
}

// validateManifests validates c for bundle manifests generation.
func (c bundleCmd) validateManifests() (err error) {
	if c.version != "" {
		if err := genutil.ValidateVersion(c.version); err != nil {
			return err
		}
	}

	// The three possible usage modes (stdin, inputDir, and legacy dirs) are mutually exclusive
	// and one must be chosen.
	isPipeReader := genutil.IsPipeReader()
	isInputDir := c.inputDir != ""
	isLegacyDirs := c.deployDir != "" || c.crdsDir != ""
	switch {
	case !(isPipeReader || isInputDir || isLegacyDirs):
		return errors.New("one of stdin, --input-dir, or --deploy-dir (and optionally --crds-dir) must be set")
	case isPipeReader && (isInputDir || isLegacyDirs):
		return errors.New("none of --input-dir, --deploy-dir, or --crds-dir may be set if reading from stdin")
	case isInputDir && isLegacyDirs:
		return errors.New("only one of --input-dir or --deploy-dir (and optionally --crds-dir) may be set if not reading from stdin")
	}

	if c.stdout {
		if c.outputDir != "" {
			return errors.New("--output-dir cannot be set if writing to stdout")
		}
	}

	return nil
}

// TODO: Move this to bundleutil package
// runManifests generates bundle manifests.
func (c bundleCmd) runManifests() (err error) {

	c.println("Generating bundle manifests")

	if !c.stdout && c.outputDir == "" {
		c.outputDir = defaultRootDir
	}

	col := &collector.Manifests{}
	switch {
	case genutil.IsPipeReader():
		err = col.UpdateFromReader(os.Stdin)
	case c.deployDir != "" && c.crdsDir != "":
		err = col.UpdateFromDirs(c.deployDir, c.crdsDir)
	case c.deployDir != "": // If only deployDir is set, use as input dir.
		c.inputDir = c.deployDir
		fallthrough
	case c.inputDir != "":
		err = col.UpdateFromDir(c.inputDir)
	}
	if err != nil {
		return err
	}

	// If no CSV was initially read, a kustomize base can be used at the default base path.
	// Only read from kustomizeDir if a base exists so users can still generate a barebones CSV.
	baseCSVPath := filepath.Join(c.kustomizeDir, "bases", c.packageName+".clusterserviceversion.yaml")
	if noCSVStdin := len(col.ClusterServiceVersions) == 0; noCSVStdin && genutil.IsExist(baseCSVPath) {
		base, err := bases.ClusterServiceVersion{BasePath: baseCSVPath}.GetBase()
		if err != nil {
			return fmt.Errorf("error reading CSV base: %v", err)
		}
		col.ClusterServiceVersions = append(col.ClusterServiceVersions, *base)
	} else if noCSVStdin {
		c.println("Building a ClusterServiceVersion without an existing base")
	}

	relatedImages, err := genutil.FindRelatedImages(col)
	if err != nil {
		return err
	}

	var opts []gencsv.Option
	stdout := genutil.NewMultiManifestWriter(os.Stdout)
	if c.stdout {
		opts = append(opts, gencsv.WithWriter(stdout))
	} else {
		opts = append(opts, gencsv.WithBundleWriter(c.outputDir))
		if c.ignoreIfOnlyCreatedAtChanged && genutil.IsExist(c.outputDir) {
			opts = append(opts, gencsv.WithBundleReader(c.outputDir))
			opts = append(opts, gencsv.WithIgnoreIfOnlyCreatedAtChanged())
		}
	}

	csvGen := gencsv.Generator{
		OperatorName:         c.packageName,
		Version:              c.version,
		Collector:            col,
		Annotations:          metricsannotations.MakeBundleObjectAnnotations(c.layout),
		ExtraServiceAccounts: c.extraServiceAccounts,
		RelatedImages:        relatedImages,
	}
	if err := csvGen.Generate(opts...); err != nil {
		return fmt.Errorf("error generating ClusterServiceVersion: %v", err)
	}

	objs := genutil.GetManifestObjects(col, c.extraServiceAccounts)
	if c.stdout {
		if err := genutil.WriteObjects(stdout, objs...); err != nil {
			return err
		}
	} else {
		dir := filepath.Join(c.outputDir, bundle.ManifestsDir)
		if err := genutil.WriteObjectsToFiles(dir, objs...); err != nil {
			return err
		}
	}

	// Pin images to digests if enabled
	if c.useImageDigests {
		c.println("pinning image versions to digests instead of tags")
		if err := c.pinImages(filepath.Join(c.outputDir, "manifests")); err != nil {
			return err
		}
	}

	// Write the scorecard config if it was passed.
	if err := writeScorecardConfig(c.outputDir, col.ScorecardConfig); err != nil {
		return fmt.Errorf("error writing bundle scorecard config: %v", err)
	}

	c.println("Bundle manifests generated successfully in", c.outputDir)

	return nil

}

// writeScorecardConfig writes cfg to dir at the hard-coded config path 'config.yaml'.
func writeScorecardConfig(dir string, cfg v1alpha3.Configuration) error {
	// Skip writing if config is empty.
	if cfg.Metadata.Name == "" {
		return nil
	}

	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	cfgDir := filepath.Join(dir, filepath.FromSlash(scorecard.DefaultConfigDir))
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		return err
	}
	scorecardConfigPath := filepath.Join(cfgDir, scorecard.ConfigFileName)
	return os.WriteFile(scorecardConfigPath, b, 0666)
}

// runMetadata generates a bundle.Dockerfile and bundle metadata.
func (c bundleCmd) runMetadata() error {
	c.println("Generating bundle metadata")
	if c.outputDir == "" {
		c.outputDir = defaultRootDir
	}

	// If metadata already exists, only overwrite it if directed to.
	bundleRoot := c.inputDir
	if bundleRoot == "" {
		bundleRoot = c.outputDir
	}

	// Find metadata from output directory only of it exists on disk.
	if genutil.IsExist(bundleRoot) {
		if _, _, err := registry.FindBundleMetadata(bundleRoot); err != nil {
			merr := registry.MetadataNotFoundError("")
			if !errors.As(err, &merr) {
				return err
			}
		} else if !c.overwrite {
			return nil
		}
	}

	scorecardConfigPath := filepath.Join(bundleRoot, scorecard.DefaultConfigDir, scorecard.ConfigFileName)

	bundleMetadata := bundleutil.BundleMetaData{
		BundleDir:            c.outputDir,
		PackageName:          c.packageName,
		Channels:             c.channels,
		DefaultChannel:       c.defaultChannel,
		OtherLabels:          metricsannotations.MakeBundleMetadataLabels(c.layout),
		IsScoreConfigPresent: genutil.IsExist(scorecardConfigPath),
	}

	return bundleMetadata.GenerateMetadata()
}

// pinImages is used to replace all image tags in the given manifests with digests
func (c bundleCmd) pinImages(manifestPath string) error {
	manifests, err := pullspec.FromDirectory(manifestPath, nil)
	if err != nil {
		return err
	}
	resolverArgs := make(map[string]string)
	resolverArgs["usedefault"] = "true"
	resolver, err := imageresolver.GetResolver(imageresolver.ResolverCrane, resolverArgs)
	if err != nil {
		return err
	}
	if err := image.Pin(resolver, manifests); err != nil {
		return err
	}

	for _, manifest := range manifests {
		if err := manifest.Dump(nil); err != nil {
			return err
		}
	}

	return nil
}
