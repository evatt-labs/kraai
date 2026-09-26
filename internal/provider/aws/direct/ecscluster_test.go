package direct

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const ecsCluster = "AWS::ECS::Cluster"

// clusterServer answers DescribeClusters by the include values the request
// carries, recording each request body.
func clusterServer(t *testing.T, respond func(include string) (int, string)) (*Client, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen = append(seen, string(raw))
		var in struct {
			Include []string `json:"include"`
		}
		_ = json.Unmarshal(raw, &in)
		include := strings.Join(in.Include, ",")
		status, body := respond(include)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP:        srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region:      "us-east-1",
		Endpoint:    func(string) string { return srv.URL },
		Now:         func() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) },
	}, &seen
}

// One call asks for tags, settings and configuration together, as a list
// input, and reads them all.
func TestReadECSCluster(t *testing.T) {
	const arn = "arn:aws:ecs:us-east-1:1:cluster/kraai"
	client, seen := clusterServer(t, func(include string) (int, string) {
		if include != "TAGS,SETTINGS,CONFIGURATIONS" {
			return 400, `{"__type":"InvalidParameterException","message":"unexpected include ` + include + `"}`
		}
		return 200, `{"clusters":[{"clusterArn":"` + arn + `","clusterName":"kraai","status":"ACTIVE",` +
			`"capacityProviders":["FARGATE","FARGATE_SPOT"],` +
			`"defaultCapacityProviderStrategy":[{"capacityProvider":"FARGATE","weight":1,"base":0}],` +
			`"tags":[{"key":"team","value":"cloud"}],` +
			`"settings":[{"name":"containerInsights","value":"disabled"}],` +
			`"configuration":{` +
			`"executeCommandConfiguration":{"kmsKeyId":"key-exec","logging":"OVERRIDE",` +
			`"logConfiguration":{"cloudWatchLogGroupName":"/ecs/kraai","cloudWatchEncryptionEnabled":true,` +
			`"s3BucketName":"kraai-logs","s3EncryptionEnabled":false,"s3KeyPrefix":"ecs"}},` +
			`"managedStorageConfiguration":{"kmsKeyId":"key-storage","fargateEphemeralStorageKmsKeyId":"key-ephemeral"}}}]}`
	})

	got, err := client.Read(context.Background(), ecsCluster, map[string]string{"ClusterName": "kraai"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"Arn":               arn,
		"ClusterName":       "kraai",
		"CapacityProviders": []any{"FARGATE", "FARGATE_SPOT"},
		"DefaultCapacityProviderStrategy": []any{map[string]any{
			"CapacityProvider": "FARGATE", "Weight": json.Number("1"), "Base": json.Number("0"),
		}},
		"Tags": []any{map[string]any{"Key": "team", "Value": "cloud"}},
		"ClusterSettings": []any{map[string]any{
			"Name": "containerInsights", "Value": "disabled",
		}},
		"Configuration": map[string]any{
			"ExecuteCommandConfiguration": map[string]any{
				"KmsKeyId": "key-exec",
				"Logging":  "OVERRIDE",
				"LogConfiguration": map[string]any{
					"CloudWatchLogGroupName":      "/ecs/kraai",
					"CloudWatchEncryptionEnabled": true,
					"S3BucketName":                "kraai-logs",
					"S3EncryptionEnabled":         false,
					"S3KeyPrefix":                 "ecs",
				},
			},
			"ManagedStorageConfiguration": map[string]any{
				"KmsKeyId":                        "key-storage",
				"FargateEphemeralStorageKmsKeyId": "key-ephemeral",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read =\n%#v\nwant\n%#v", got, want)
	}
	want1 := []string{`{"clusters":["kraai"],"include":["TAGS","SETTINGS","CONFIGURATIONS"]}`}
	if !reflect.DeepEqual(*seen, want1) {
		t.Fatalf("requests = %v, want %v", *seen, want1)
	}
}

// A cluster whose status says it is gone is absent, although the service
// still describes it.
func TestReadECSClusterAbsentByStatus(t *testing.T) {
	client, _ := clusterServer(t, func(string) (int, string) {
		return 200, `{"clusters":[{"clusterArn":"arn:aws:ecs:us-east-1:1:cluster/gone","clusterName":"gone","status":"INACTIVE"}]}`
	})
	_, err := client.Read(context.Background(), ecsCluster, map[string]string{"ClusterName": "gone"})
	if !errors.Is(err, ErrAbsent) {
		t.Fatalf("Read error = %v, want ErrAbsent", err)
	}
}

// DescribeClusters never errors on an unknown name; a name that matches no
// cluster answers with an empty list, which is absence.
func TestReadECSClusterEmptyListIsAbsent(t *testing.T) {
	client, _ := clusterServer(t, func(string) (int, string) {
		return 200, `{"clusters":[],"failures":[{"arn":"kraai-nonexistent-cluster-zzz","reason":"MISSING"}]}`
	})
	_, err := client.Read(context.Background(), ecsCluster, map[string]string{"ClusterName": "kraai-nonexistent-cluster-zzz"})
	if !errors.Is(err, ErrAbsent) {
		t.Fatalf("Read error = %v, want ErrAbsent", err)
	}
}
