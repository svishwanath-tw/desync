package main

import (
	"fmt"
	"net/url"
	"strings"

	"path"

	"github.com/folbricht/desync"
	"github.com/pkg/errors"
)

// cmdStoreOptions are used to pass additional options to store initalization from the
// commandline. These generally override settings from the config file.
type cmdStoreOptions struct {
	n          int
	clientCert string
	clientKey  string
	skipVerify bool
}

// MergeWith takes store options as read from the config, and applies command-line
// provided options on top of them and returns the merged result.
func (o cmdStoreOptions) MergedWith(opt desync.StoreOptions) desync.StoreOptions {
	opt.N = o.n
	if o.clientCert != "" {
		opt.ClientCert = o.clientCert
	}
	if o.clientKey != "" {
		opt.ClientKey = o.clientKey
	}
	if o.skipVerify {
		opt.SkipVerify = true
	}
	return opt
}

// MultiStoreWithCache is used to parse store and cache locations given in the
// command line.
// cacheLocation - Place of the local store used for caching, can be blank
// storeLocation - URLs or paths to remote or local stores that should be queried in order
func MultiStoreWithCache(cmdOpt cmdStoreOptions, cacheLocation string, storeLocations ...string) (desync.Store, error) {
	var (
		store  desync.Store
		stores []desync.Store
	)
	for _, location := range storeLocations {
		s, err := storeFromLocation(location, cmdOpt)
		if err != nil {
			return store, err
		}
		stores = append(stores, s)
	}

	// Combine all stores into one router
	store = desync.NewStoreRouter(stores...)

	// See if we want to use a writable store as cache, if so, attach a cache to
	// the router
	if cacheLocation != "" {
		cache, err := WritableStore(cacheLocation, cmdOpt)
		if err != nil {
			return store, err
		}

		if ls, ok := cache.(desync.LocalStore); ok {
			ls.UpdateTimes = true
		}
		store = desync.NewCache(store, cache)
	}
	return store, nil
}

// multiStoreWithCache is used to parse store locations, and return a store
// router instance containing them all for reading, in the order they're given
func multiStore(cmdOpt cmdStoreOptions, storeLocations ...string) (desync.Store, error) {
	var stores []desync.Store
	for _, location := range storeLocations {
		s, err := storeFromLocation(location, cmdOpt)
		if err != nil {
			return nil, err
		}
		stores = append(stores, s)
	}

	return desync.NewStoreRouter(stores...), nil
}

// WritableStore is used to parse a store location from the command line for
// commands that expect to write chunks, such as make or tar. It determines
// which type of writable store is needed, instantiates and returns a
// single desync.WriteStore.
func WritableStore(location string, cmdOpt cmdStoreOptions) (desync.WriteStore, error) {
	s, err := storeFromLocation(location, cmdOpt)
	if err != nil {
		return nil, err
	}
	store, ok := s.(desync.WriteStore)
	if !ok {
		return nil, fmt.Errorf("store '%s' does not support writing", location)
	}
	return store, nil
}

// Parse a single store URL or path and return an initialized instance of it
func storeFromLocation(location string, cmdOpt cmdStoreOptions) (desync.Store, error) {
	loc, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("Unable to parse store location %s : %s", location, err)
	}

	// Get any store options from the config if present and overwrite with settings from
	// the command line
	opt := cmdOpt.MergedWith(cfg.GetStoreOptionsFor(location))

	var s desync.Store
	switch loc.Scheme {
	case "ssh":
		s, err = desync.NewRemoteSSHStore(loc, opt)
		if err != nil {
			return nil, err
		}
	case "sftp":
		s, err = desync.NewSFTPStore(loc, opt)
		if err != nil {
			return nil, err
		}
	case "http", "https":
		// This is for backwards compatibility only, to support http-timeout and
		// http-error-retry in the config file for a bit longer. If those are
		// set, and the options for the store aren't, then use the old values.
		// TODO: Remove this code and drop these config options in the future.
		if opt.Timeout == 0 && cfg.HTTPTimeout > 0 {
			opt.Timeout = cfg.HTTPTimeout
		}
		if opt.ErrorRetry == 0 && cfg.HTTPErrorRetry > 0 {
			opt.ErrorRetry = cfg.HTTPErrorRetry
		}
		s, err = desync.NewRemoteHTTPStore(loc, opt)
		if err != nil {
			return nil, err
		}
	case "s3+http", "s3+https":
		s3Creds, region := cfg.GetS3CredentialsFor(loc)
		s, err = desync.NewS3Store(loc, s3Creds, region, opt)
		if err != nil {
			return nil, err
		}
	case "":
		s, err = desync.NewLocalStore(loc.Path, opt)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("Unsupported store access scheme %s", loc.Scheme)
	}
	return s, nil
}

func readCaibxFile(location string, cmdOpt cmdStoreOptions) (c desync.Index, err error) {
	is, indexName, err := indexStoreFromLocation(location, cmdOpt)
	if err != nil {
		return c, err
	}
	defer is.Close()

	return is.GetIndex(indexName)
}

func storeCaibxFile(idx desync.Index, location string, cmdOpt cmdStoreOptions) error {
	is, indexName, err := writableIndexStore(location, cmdOpt)
	if err != nil {
		return err
	}
	defer is.Close()
	return is.StoreIndex(indexName, idx)
}

// WritableIndexStore is used to parse a store location from the command line for
// commands that expect to write indexes, such as make or tar. It determines
// which type of writable store is needed, instantiates and returns a
// single desync.IndexWriteStore.
func writableIndexStore(location string, cmdOpt cmdStoreOptions) (desync.IndexWriteStore, string, error) {
	s, indexName, err := indexStoreFromLocation(location, cmdOpt)
	if err != nil {
		return nil, indexName, err
	}
	store, ok := s.(desync.IndexWriteStore)
	if !ok {
		return nil, indexName, fmt.Errorf("index store '%s' does not support writing", location)
	}
	return store, indexName, nil
}

// Parse a single store URL or path and return an initialized instance of it
func indexStoreFromLocation(location string, cmdOpt cmdStoreOptions) (desync.IndexStore, string, error) {
	loc, err := url.Parse(location)
	if err != nil {
		return nil, "", fmt.Errorf("Unable to parse store location %s : %s", location, err)
	}

	indexName := path.Base(loc.Path)
	// Remove file name from url path
	p := *loc
	p.Path = path.Dir(p.Path)

	// Get any store options from the config if present and overwrite with settings from
	// the command line. FIXME: This way of breaking off the file path from an index URL
	// won't work on Windows when working with local stores.
	opt := cmdOpt.MergedWith(cfg.GetStoreOptionsFor(location[:strings.LastIndex(location, "/")]))

	var s desync.IndexStore
	switch loc.Scheme {
	case "ssh":
		return nil, "", errors.New("Index storage is not supported by ssh remote stores")
	case "sftp":
		s, err = desync.NewSFTPIndexStore(loc, opt)
		if err != nil {
			return nil, "", err
		}
	case "http", "https":
		// This is for backwards compatibility only, to support http-timeout and
		// http-error-retry in the config file for a bit longer. If those are
		// set, and the options for the store aren't, then use the old values.
		// TODO: Remove this code and drop these config options in the future.
		if opt.Timeout == 0 && cfg.HTTPTimeout > 0 {
			opt.Timeout = cfg.HTTPTimeout
		}
		if opt.ErrorRetry == 0 && cfg.HTTPErrorRetry > 0 {
			opt.ErrorRetry = cfg.HTTPErrorRetry
		}
		s, err = desync.NewRemoteHTTPIndexStore(&p, opt)
		if err != nil {
			return nil, "", err
		}
	case "s3+http", "s3+https":
		s3Creds, region := cfg.GetS3CredentialsFor(&p)
		s, err = desync.NewS3IndexStore(&p, s3Creds, region, opt)
		if err != nil {
			return nil, "", err
		}
	case "":
		if location == "-" {
			s, _ = desync.NewConsoleIndexStore()
		} else {
			s, err = desync.NewLocalIndexStore(p.String())
			if err != nil {
				return nil, "", err
			}
		}
	default:
		return nil, "", fmt.Errorf("Unsupported store access scheme %s", loc.Scheme)
	}
	return s, indexName, nil
}
