//
// Copyright 2023 The GUAC Authors.
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

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/guacsec/guac/pkg/assembler"
	"github.com/guacsec/guac/pkg/assembler/graphdb"
	"github.com/guacsec/guac/pkg/emitter"
	"github.com/guacsec/guac/pkg/handler/collector"
	"github.com/guacsec/guac/pkg/handler/collector/file"
	"github.com/guacsec/guac/pkg/handler/processor"
	"github.com/guacsec/guac/pkg/handler/processor/process"
	"github.com/guacsec/guac/pkg/ingestor/parser"
	"github.com/guacsec/guac/pkg/logging"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var flags = struct {
	dbAddr  string
	gdbuser string
	gdbpass string
	realm   string
	keyPath string
	keyID   string
}{}

type options struct {
	dbAddr string
	user   string
	pass   string
	realm  string
	// path to the pem file
	keyPath string
	// ID related to the key being stored
	keyID string
	// path to folder with documents to collect
	path string
}

var filesCmd = &cobra.Command{
	Use:   "files [flags] file_path",
	Short: "take a folder of files and create a GUAC graph utilizing Nats pubsub",
	Run: func(cmd *cobra.Command, args []string) {

		opts, err := validateFlags(
			viper.GetString("gdbuser"),
			viper.GetString("gdbpass"),
			viper.GetString("gdbaddr"),
			viper.GetString("realm"),
			viper.GetString("verifier-keyPath"),
			viper.GetString("verifier-keyID"),
			args)
		if err != nil {
			fmt.Printf("unable to validate flags: %v\n", err)
			_ = cmd.Help()
			os.Exit(1)
		}

		ctx := logging.WithLogger(context.Background())
		logger := logging.FromContext(ctx)

		// Register collector
		fileCollector := file.NewFileCollector(ctx, opts.path, false, time.Second)
		err = collector.RegisterDocumentCollector(fileCollector, file.FileCollector)
		if err != nil {
			logger.Errorf("unable to register file collector: %v", err)
		}

		// initialize jetstream
		// TODO: pass in credentials file for NATS secure login
		jetStream := emitter.NewJetStream(nats.DefaultURL, "", "")
		ctx, err = jetStream.JetStreamInit(ctx)
		if err != nil {
			logger.Errorf("jetStream initialization failed with error: %v", err)
			os.Exit(1)
		}
		// recreate stream to remove any old lingering documents
		// NOT TO BE USED IN PRODUCTION
		err = jetStream.RecreateStream(ctx)
		if err != nil {
			logger.Errorf("unexpected error recreating jetstream: %v", err)
		}
		defer jetStream.Close()

		// Get pipeline of components
		collectorPubFunc, err := getCollectorPublish(ctx)
		if err != nil {
			logger.Errorf("error: %v", err)
			os.Exit(1)
		}

		assemblerFunc, err := getAssembler(opts)
		if err != nil {
			logger.Errorf("error: %v", err)
			os.Exit(1)
		}

		processorTransportFunc := func(d processor.DocumentTree) error {
			docTreeBytes, err := json.Marshal(d)
			if err != nil {
				return fmt.Errorf("failed marshal of document: %w", err)
			}
			err = emitter.Publish(ctx, emitter.SubjectNameDocProcessed, docTreeBytes)
			if err != nil {
				return err
			}
			return nil
		}

		ingestorTransportFunc := func(d []assembler.Graph) error {
			err := assemblerFunc(d)
			if err != nil {
				return err
			}
			return nil
		}

		processorFunc, err := getProcessor(ctx, processorTransportFunc)
		if err != nil {
			logger.Errorf("error: %v", err)
			os.Exit(1)
		}
		ingestorFunc, err := getIngestor(ctx, ingestorTransportFunc)
		if err != nil {
			logger.Errorf("error: %v", err)
			os.Exit(1)
		}

		// Set emit function to go through the entire pipeline
		emit := func(d *processor.Document) error {
			err = collectorPubFunc(d)
			if err != nil {
				logger.Errorf("collector ended with error: %v", err)
				os.Exit(1)
			}
			return nil
		}

		// Collect
		errHandler := func(err error) bool {
			if err == nil {
				logger.Info("collector ended gracefully")
				return true
			}
			logger.Errorf("collector ended with error: %v", err)
			return false
		}

		// Assuming that publisher and consumer are different processes.
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := processorFunc()
			if err != nil {
				logger.Errorf("processor ended with error: %v", err)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := ingestorFunc()
			if err != nil {
				logger.Errorf("parser ended with error: %v", err)
			}
		}()

		if err := collector.Collect(ctx, emit, errHandler); err != nil {
			logger.Fatal(err)
		}

		wg.Wait()
	},
}

func validateFlags(user string, pass string, dbAddr string, realm string, keyPath string, keyID string, args []string) (options, error) {
	var opts options
	opts.user = user
	opts.pass = pass
	opts.dbAddr = dbAddr
	opts.realm = realm

	if keyPath != "" {
		if strings.HasSuffix(keyPath, "pem") {
			opts.keyPath = keyPath
		} else {
			return opts, errors.New("key must be passed in as a pem file")
		}
	}
	if keyPath != "" {
		opts.keyID = keyID
	}

	if len(args) != 1 {
		return opts, fmt.Errorf("expected positional argument for file_path")
	}

	opts.path = args[0]

	return opts, nil
}

func getCollectorPublish(ctx context.Context) (func(*processor.Document) error, error) {
	return func(d *processor.Document) error {
		return collector.Publish(ctx, d)
	}, nil
}

func getProcessor(ctx context.Context, transportFunc func(processor.DocumentTree) error) (func() error, error) {
	return func() error {
		return process.Subscribe(ctx, transportFunc)
	}, nil
}

func getIngestor(ctx context.Context, transportFunc func([]assembler.Graph) error) (func() error, error) {
	return func() error {
		err := parser.Subscribe(ctx, transportFunc)
		if err != nil {
			return err
		}
		return nil
	}, nil
}

func getAssembler(opts options) (func([]assembler.Graph) error, error) {
	authToken := graphdb.CreateAuthTokenWithUsernameAndPassword(
		opts.user,
		opts.pass,
		opts.realm,
	)

	client, err := graphdb.NewGraphClient(opts.dbAddr, authToken)
	if err != nil {
		return nil, err
	}

	err = createIndices(client)
	if err != nil {
		return nil, err
	}

	return func(gs []assembler.Graph) error {
		combined := assembler.Graph{
			Nodes: []assembler.GuacNode{},
			Edges: []assembler.GuacEdge{},
		}
		for _, g := range gs {
			combined.AppendGraph(g)
		}
		if err := assembler.StoreGraph(combined, client); err != nil {
			return err
		}

		return nil
	}, nil
}

func createIndices(client graphdb.Client) error {
	indices := map[string][]string{
		"Artifact":      {"digest", "name"},
		"Package":       {"purl", "name"},
		"Metadata":      {"id"},
		"Attestation":   {"digest"},
		"Vulnerability": {"id"},
	}

	for label, attributes := range indices {
		for _, attribute := range attributes {
			err := assembler.CreateIndexOn(client, label, attribute)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func init() {
	rootCmd.AddCommand(filesCmd)
}
