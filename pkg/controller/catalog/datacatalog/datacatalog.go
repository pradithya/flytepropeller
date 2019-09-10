package datacatalog

import (
	"context"
	"crypto/x509"
	"time"

	"fmt"

	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	datacatalog "github.com/lyft/datacatalog/protos/gen"
	"github.com/lyft/flyteidl/gen/pb-go/flyteidl/core"
	"github.com/lyft/flytepropeller/pkg/controller/catalog/datacatalog/transformer"
	"github.com/lyft/flytestdlib/logger"
	"github.com/lyft/flytestdlib/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/uuid"
)

const (
	taskVersionKey = "task-version"
	taskExecKey    = "execution-name"
)

// This is the client that caches task executions to DataCatalog service.
type CatalogClient struct {
	client datacatalog.DataCatalogClient
	store  storage.ProtobufStore
}

func (m *CatalogClient) getArtifactByTag(ctx context.Context, tagName string, dataset *datacatalog.Dataset) (*datacatalog.Artifact, error) {
	logger.Debugf(ctx, "Get Artifact by tag %v", tagName)
	artifactQuery := &datacatalog.GetArtifactRequest{
		Dataset: dataset.Id,
		QueryHandle: &datacatalog.GetArtifactRequest_TagName{
			TagName: tagName,
		},
	}
	response, err := m.client.GetArtifact(ctx, artifactQuery)
	if err != nil {
		return nil, err
	}

	return response.Artifact, nil
}

func (m *CatalogClient) getDataset(ctx context.Context, task *core.TaskTemplate) (*datacatalog.Dataset, error) {
	datasetID, err := transformer.GenerateDatasetIDForTask(ctx, task)
	if err != nil {
		return nil, err
	}
	logger.Debugf(ctx, "Get Dataset %v", datasetID)

	dsQuery := &datacatalog.GetDatasetRequest{
		Dataset: datasetID,
	}

	datasetResponse, err := m.client.GetDataset(ctx, dsQuery)
	if err != nil {
		return nil, err
	}

	return datasetResponse.Dataset, nil
}

func (m *CatalogClient) validateTask(task *core.TaskTemplate) error {
	taskInterface := task.Interface
	if taskInterface == nil {
		return fmt.Errorf("Task interface cannot be nil, task: [%+v]", task)
	}

	if task.Id == nil {
		return fmt.Errorf("Task ID cannot be nil, task: [%+v]", task)
	}

	if task.Metadata == nil {
		return fmt.Errorf("Task metadata cannot be nil, task: [%+v]", task)
	}

	return nil
}

// Get the cached task execution from Catalog.
// These are the steps taken:
// - Verify there is a Dataset created for the Task
// - Lookup the Artifact that is tagged with the hash of the input values
// - The artifactData contains the literal values that serve as the task outputs
func (m *CatalogClient) Get(ctx context.Context, task *core.TaskTemplate, inputPath storage.DataReference) (*core.LiteralMap, error) {
	inputs := &core.LiteralMap{}

	if err := m.validateTask(task); err != nil {
		logger.Errorf(ctx, "DataCatalog task validation failed %+v, err: %+v", task, err)
		return nil, err
	}

	if task.Interface.Inputs != nil && len(task.Interface.Inputs.Variables) != 0 {
		if err := m.store.ReadProtobuf(ctx, inputPath, inputs); err != nil {
			logger.Errorf(ctx, "DataCatalog failed to read inputs %+v, err: %+v", inputPath, err)
			return nil, err
		}
		logger.Debugf(ctx, "DataCatalog read inputs from %v", inputPath)
	}

	dataset, err := m.getDataset(ctx, task)
	if err != nil {
		logger.Errorf(ctx, "DataCatalog failed to get dataset for task %+v, err: %+v", task, err)
		return nil, err
	}

	tag, err := transformer.GenerateArtifactTagName(ctx, inputs)
	if err != nil {
		logger.Errorf(ctx, "DataCatalog failed to generate tag for inputs %+v, err: %+v", inputs, err)
		return nil, err
	}

	artifact, err := m.getArtifactByTag(ctx, tag, dataset)
	if err != nil {
		logger.Errorf(ctx, "DataCatalog failed to get artifact by tag %+v, err: %+v", tag, err)
		return nil, err
	}
	logger.Debugf(ctx, "Artifact found %v from tag %v", artifact, tag)

	outputs, err := transformer.GenerateTaskOutputsFromArtifact(task, artifact)
	if err != nil {
		logger.Errorf(ctx, "DataCatalog failed to get outputs from artifact %+v, err: %+v", artifact.Id, err)
		return nil, err
	}

	logger.Debugf(ctx, "Cached %v artifact outputs from artifact %v", len(outputs.Literals), artifact.Id)
	return outputs, nil
}

// Catalog the task execution as a cached Artifact. We associate an Artifact as the cached data by tagging the Artifact
// with the hash of the input values.
//
// The steps taken to cache an execution:
// - Ensure a Dataset exists for the Artifact. The Dataset represents the proj/domain/name/version of the task
// - Create an Artifact with the execution data that belongs to the dataset
// - Tag the Artifact with a hash generated by the input values
func (m *CatalogClient) Put(ctx context.Context, task *core.TaskTemplate, execID *core.TaskExecutionIdentifier, inputPath storage.DataReference, outputPath storage.DataReference) error {
	inputs := &core.LiteralMap{}
	outputs := &core.LiteralMap{}

	if err := m.validateTask(task); err != nil {
		logger.Errorf(ctx, "DataCatalog task validation failed %+v, err: %+v", task, err)
		return err
	}

	if task.Interface.Inputs != nil && len(task.Interface.Inputs.Variables) != 0 {
		if err := m.store.ReadProtobuf(ctx, inputPath, inputs); err != nil {
			logger.Errorf(ctx, "DataCatalog failed to read inputs %+v, err: %+v", inputPath, err)
			return err
		}
		logger.Debugf(ctx, "DataCatalog read inputs from %v", inputPath)
	}

	if task.Interface.Outputs != nil && len(task.Interface.Outputs.Variables) != 0 {
		if err := m.store.ReadProtobuf(ctx, outputPath, outputs); err != nil {
			logger.Errorf(ctx, "DataCatalog failed to read outputs %+v, err: %+v", outputPath, err)
			return err
		}
		logger.Debugf(ctx, "DataCatalog read outputs from %v", outputPath)
	}

	datasetID, err := transformer.GenerateDatasetIDForTask(ctx, task)
	if err != nil {
		logger.Errorf(ctx, "DataCatalog failed to generate dataset for task %+v, err: %+v", task, err)
		return err
	}

	logger.Debugf(ctx, "DataCatalog put into Catalog for DataSet %v", datasetID)

	// Try creating the dataset in case it doesn't exist

	metadata := &datacatalog.Metadata{
		KeyMap: map[string]string{
			taskVersionKey: task.Id.Version,
			taskExecKey:    execID.NodeExecutionId.NodeId,
		},
	}
	newDataset := &datacatalog.Dataset{
		Id:       datasetID,
		Metadata: metadata,
	}

	_, err = m.client.CreateDataset(ctx, &datacatalog.CreateDatasetRequest{Dataset: newDataset})
	if err != nil {
		logger.Debugf(ctx, "Create dataset %v return err %v", datasetID, err)

		if status.Code(err) == codes.AlreadyExists {
			logger.Debugf(ctx, "Create Dataset for task %v already exists", task.Id)
		} else {
			logger.Errorf(ctx, "Unable to create dataset %+v, err: %+v", datasetID, err)
			return err
		}
	}

	// Create the artifact for the execution that belongs in the task
	artifactDataList := make([]*datacatalog.ArtifactData, 0, len(outputs.Literals))
	for name, value := range outputs.Literals {
		artifactData := &datacatalog.ArtifactData{
			Name:  name,
			Value: value,
		}
		artifactDataList = append(artifactDataList, artifactData)
	}

	cachedArtifact := &datacatalog.Artifact{
		Id:       string(uuid.NewUUID()),
		Dataset:  datasetID,
		Data:     artifactDataList,
		Metadata: metadata,
	}

	createArtifactRequest := &datacatalog.CreateArtifactRequest{Artifact: cachedArtifact}
	_, err = m.client.CreateArtifact(ctx, createArtifactRequest)
	if err != nil {
		logger.Errorf(ctx, "Failed to create Artifact %+v, err: %v", cachedArtifact, err)
		return err
	}
	logger.Debugf(ctx, "Created artifact: %v, with %v outputs from execution %v", cachedArtifact.Id, len(artifactDataList), execID.TaskId.Name)

	// Tag the artifact since it is the cached artifact
	tagName, err := transformer.GenerateArtifactTagName(ctx, inputs)
	if err != nil {
		logger.Errorf(ctx, "Failed to create tag for artifact %+v, err: %+v", cachedArtifact.Id, err)
		return err
	}
	logger.Debugf(ctx, "Created tag: %v, for task: %v", tagName, task.Id)

	// TODO: We should create the artifact + tag in a transaction when the service supports that
	tag := &datacatalog.Tag{
		Name:       tagName,
		Dataset:    datasetID,
		ArtifactId: cachedArtifact.Id,
	}
	_, err = m.client.AddTag(ctx, &datacatalog.AddTagRequest{Tag: tag})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			logger.Errorf(ctx, "Tag %v already exists for Artifact %v (idempotent)", tagName, cachedArtifact.Id)
		}

		logger.Errorf(ctx, "Failed to add tag %+v for artifact %+v, err: %+v", tagName, cachedArtifact.Id, err)
		return err
	}

	return nil
}

func NewDataCatalog(ctx context.Context, endpoint string, insecureConnection bool, datastore storage.ProtobufStore) (*CatalogClient, error) {
	var opts []grpc.DialOption

	grpcOptions := []grpc_retry.CallOption{
		grpc_retry.WithBackoff(grpc_retry.BackoffLinear(100 * time.Millisecond)),
		grpc_retry.WithCodes(codes.DeadlineExceeded, codes.Unavailable, codes.Canceled),
		grpc_retry.WithMax(5),
	}

	if insecureConnection {
		logger.Debug(ctx, "Establishing insecure connection to DataCatalog")
		opts = append(opts, grpc.WithInsecure())
	} else {
		logger.Debug(ctx, "Establishing secure connection to DataCatalog")
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}

		creds := credentials.NewClientTLSFromCert(pool, "")
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	retryInterceptor := grpc.WithUnaryInterceptor(grpc_retry.UnaryClientInterceptor(grpcOptions...))

	opts = append(opts, retryInterceptor)
	clientConn, err := grpc.Dial(endpoint, opts...)
	if err != nil {
		return nil, err
	}

	client := datacatalog.NewDataCatalogClient(clientConn)

	return &CatalogClient{
		client: client,
		store:  datastore,
	}, nil
}
