package pkg

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/caarlos0/log"
	"github.com/go-viper/mapstructure/v2"
	"github.com/marcosnils/bin/pkg/assets"
	"github.com/marcosnils/bin/pkg/config"
	"github.com/marcosnils/bin/pkg/providers"
)

func DoInstall(u string, provider string, path string, fetchOptsFields map[string]any, force bool, all bool) error {
	p, err := providers.New(u, provider)
	if err != nil {
		return err
	}
	log.Debugf("Using provider '%s' for '%s'", p.GetID(), u)

	var fetchOpts providers.FetchOpts
	if err := mapstructure.Decode(fetchOptsFields, &fetchOpts); err != nil {
		panic(err)
	}

	pResults, err := p.Fetch(&fetchOpts)
	if err != nil {
		return err
	}

	basePath := path
	// If multiple results, path must be a directory.
	// If path is a file, use its directory as base.
	if len(pResults) > 1 {
		fi, err := os.Stat(os.ExpandEnv(path))
		if err == nil && !fi.IsDir() {
			basePath = filepath.Dir(path)
		}
	}

	for _, pResult := range pResults {
		var finalPath string
		if len(pResults) > 1 {
			finalPath = filepath.Join(basePath, pResult.Name)
		} else {
			finalPath, err = checkFinalPath(path, assets.SanitizeName(pResult.Name, pResult.Version))
			if err != nil {
				return err
			}
		}

		hash, err := saveToDisk(pResult, finalPath, force)
		if err != nil {
			return fmt.Errorf("error installing binary: %w", err)
		}

		// Convert to absolute path before storing in config
		absPath, err := filepath.Abs(finalPath)
		if err != nil {
			return fmt.Errorf("error converting to absolute path: %w", err)
		}

		bin := &config.Binary{
			RemoteName:  pResult.Name,
			Path:        absPath,
			Version:     pResult.Version,
			Hash:        fmt.Sprintf("%x", hash),
			URL:         u,
			Provider:    p.GetID(),
			PackagePath: pResult.PackagePath,
		}

		// When ensuring, we might be overwriting a pinned binary, so we need to preserve the pin
		if existing := config.Get().Bins[absPath]; existing != nil {
			bin.Pinned = existing.Pinned
		}

		err = config.UpsertBinary(bin)
		if err != nil {
			return err
		}

		log.Infof("Done installing %s %s", pResult.Name, pResult.Version)
	}

	return nil
}

// checkFinalPath checks if path exists and if it's a dir or not
// and returns the correct final file path. It also
// checks if the path already exists and prompts
// the user to override
func checkFinalPath(path, fileName string) (string, error) {
	fi, err := os.Stat(os.ExpandEnv(path))

	// TODO implement file existence and override logic
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	if fi != nil && fi.IsDir() {
		return filepath.Join(path, fileName), nil
	}

	return path, nil
}

// saveToDisk saves the specified binary to the desired path
// and makes it executable. It also checks if any other binary
// has the same hash and exists if so.

// TODO check if other binary has the same hash and warn about it.
// TODO if the file is zipped, tared, whatever then extract it
func saveToDisk(f *providers.File, path string, overwrite bool) ([]byte, error) {
	epath := os.ExpandEnv((path))

	extraFlags := os.O_EXCL

	if overwrite {
		extraFlags = 0
		err := os.Remove(epath)
		log.Debugf("Overwrite flag set, removing file %s", epath)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	file, err := os.OpenFile(epath, os.O_RDWR|os.O_CREATE|extraFlags, 0o766)
	if err != nil {
		return nil, err
	}

	defer file.Close()

	h := sha256.New()

	tr := io.TeeReader(f.Data, h)

	log.Infof("Copying for %s@%s into %s", f.Name, f.Version, epath)
	_, err = io.Copy(file, tr)
	if err != nil {
		return nil, err
	}

	return h.Sum(nil), nil
}
