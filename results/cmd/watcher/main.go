/*
Copyright 2020 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"

	"github.com/tektoncd/experimental/results/pkg/convert"
	pb "github.com/tektoncd/experimental/results/proto/proto"
	"github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	listers "github.com/tektoncd/pipeline/pkg/client/listers/pipeline/v1beta1"
	"go.uber.org/zap"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/cache"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/injection/sharedmain"
	"knative.dev/pkg/signals"
	_ "knative.dev/pkg/system/testing"
)

var (
	apiAddr = flag.String("api_addr", "localhost:50051", "Address of API server to report to")
)

const (
	path   = "/metadata/annotations/results.tekton.dev~1id"
	idName = "results.tekton.dev/id"
)

func main() {
	flag.Parse()
	//Set up a connection to the server.
	conn, err := grpc.Dial(*apiAddr, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()
	sharedmain.MainWithContext(injection.WithNamespaceScope(signals.NewContext(), ""), "watcher", func(ctx context.Context, cmw configmap.Watcher) *controller.Impl {
		client := pb.NewResultsClient(conn)
		return newController(ctx, cmw, client)
	})
}

type reconciler struct {
	logger            *zap.SugaredLogger
	client            pb.ResultsClient
	taskRunLister     listers.TaskRunLister
	pipelineclientset versioned.Interface
}

func (r *reconciler) Reconcile(ctx context.Context, key string) error {
	r.logger.Infof("reconciling resource key: %s", key)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		r.logger.Errorf("invalid resource key: %s", key)
		return nil
	}

	// Get the Task Run resource with this namespace/name
	tr, err := r.pipelineclientset.TektonV1beta1().TaskRuns(namespace).Get(name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		// The resource no longer exists, in which case we stop processing.
		r.logger.Infof("task run %q in work queue no longer exists", key)
		return nil
	} else if err != nil {
		r.logger.Errorf("Error retrieving Result %q: %s", name, err)
		return err
	}

	r.logger.Infof("Receiving new Result %s/%s", namespace, tr.Name)

	// Send the new status of the Result to the API server.
	p, err := convert.ToProto(tr)
	if err != nil {
		r.logger.Errorf("Error converting to proto: %v", err)
		return err
	}
	res := &pb.Result{
		Executions: []*pb.Execution{{
			Execution: &pb.Execution_TaskRun{p},
		}},
	}

	// Create a Result if it does not exist in results server, update existing one otherwise.
	if val, ok := p.GetMetadata().GetAnnotations()[idName]; ok {
		res.Name = val
		if _, err := r.client.UpdateResult(ctx, &pb.UpdateResultRequest{
			Name:   val,
			Result: res,
		}); err != nil {
			r.logger.Errorf("Error updating TaskRun %s: %v", name, err)
			return err
		}
		r.logger.Infof("Sending updates for TaskRun %s/%s (result: %s)", namespace, tr.Name, val)
	} else {
		res, err = r.client.CreateResult(ctx, &pb.CreateResultRequest{
			Result: res,
		})
		if err != nil {
			r.logger.Error("Error creating Result %s: %v", name, err)
			return err
		}
		path, err := annotationPath(res.GetName(), path, "add")
		if err != nil {
			r.logger.Error("Error jsonpatch for Result %s : %v", name, err)
			return err
		}
		r.pipelineclientset.TektonV1beta1().TaskRuns(namespace).Patch(name, types.JSONPatchType, path)
		r.logger.Infof("Creating a new TaskRun result %s/%s (result: %s)", namespace, tr.Name, res.GetName())
	}
	return nil
}

// AnnotationPath creates a jsonpatch path used for adding results_id to Result annotations field.
func annotationPath(val string, path string, op string) ([]byte, error) {
	patches := []jsonpatch.JsonPatchOperation{{
		Operation: op,
		Path:      path,
		Value:     val,
	}}
	return json.Marshal(patches)
}
