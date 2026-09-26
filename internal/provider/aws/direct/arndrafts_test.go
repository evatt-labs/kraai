package direct

import "testing"

// join_main bound each of these types' primary identifier, an ARN,
// straight to an input member that takes only the name or id, a draft
// every direct read would reject. Each is now read by substituting the
// name or id out of the ARN through an {ArnProperty:arnName} input
// placeholder; the ARN itself remains the reader's identifier, bound
// through the placeholder rather than an input member of its own.
func TestReadersSubstituteARNDrafts(t *testing.T) {
	for typeName, c := range map[string]struct {
		arnProperty, member, arn, want string
	}{
		"AWS::BedrockAgentCore::ApiKeyCredentialProvider": {
			"CredentialProviderArn", "name",
			"arn:aws:bedrock-agentcore:us-east-1:123456789012:token-vault/default/apikeycredentialprovider/my-provider",
			"my-provider",
		},
		"AWS::BedrockAgentCore::PolicyEngine": {
			"PolicyEngineArn", "policyEngineId",
			"arn:aws:bedrock-agentcore:us-east-1:123456789012:policy-engine/MyPolicyEngine-ab12cd34ef",
			"MyPolicyEngine-ab12cd34ef",
		},
		"AWS::Chime::VoiceConnector": {
			"VoiceConnectorArn", "VoiceConnectorId",
			"arn:aws:chime:us-east-1:123456789012:vc/abcdef1ghij2klmno3pqr4",
			"abcdef1ghij2klmno3pqr4",
		},
		"AWS::DataExchange::DataSet": {
			"Arn", "DataSetId",
			"arn:aws:dataexchange:us-east-1:123456789012:data-sets/4b04f8d0d3b342249850d1f5e0f5d5e5",
			"4b04f8d0d3b342249850d1f5e0f5d5e5",
		},
		"AWS::HealthAgent::Domain": {
			"Arn", "domainId",
			"arn:aws:health-agent:us-east-1:123456789012:domain/dom-abc123",
			"dom-abc123",
		},
		"AWS::MediaPackageV2::ChannelGroup": {
			"Arn", "ChannelGroupName",
			"arn:aws:mediapackagev2:us-west-2:123456789012:channelGroup/exampleChannelGroup",
			"exampleChannelGroup",
		},
		"AWS::NovaAct::WorkflowDefinition": {
			"Arn", "workflowDefinitionName",
			"arn:aws:nova-act:us-east-1:123456789012:workflow-definition/my-workflow",
			"my-workflow",
		},
		"AWS::MGN::NetworkMigrationDefinition": {
			"Arn", "networkMigrationDefinitionID",
			"arn:aws:mgn:us-east-1:123456789012:network-migration-definition/nmd-0123456789abcdef0",
			"nmd-0123456789abcdef0",
		},
		"AWS::Omics::RunCache": {
			"Arn", "id",
			"arn:aws:omics:us-east-1:123456789012:runCache/1234567",
			"1234567",
		},
		"AWS::PCS::Cluster": {
			"Arn", "clusterIdentifier",
			"arn:aws:pcs:us-east-1:123456789012:cluster/pcs_1a2b3c4d5e",
			"pcs_1a2b3c4d5e",
		},
		"AWS::Proton::EnvironmentAccountConnection": {
			"Arn", "id",
			"arn:aws:proton:us-east-1:123456789012:environment-account-connection/12345678-1234-1234-1234-123456789012",
			"12345678-1234-1234-1234-123456789012",
		},
		"AWS::Proton::EnvironmentTemplate": {
			"Arn", "name",
			"arn:aws:proton:us-east-1:123456789012:environment-template/my-template",
			"my-template",
		},
		"AWS::Proton::ServiceTemplate": {
			"Arn", "name",
			"arn:aws:proton:us-east-1:123456789012:service-template/my-template",
			"my-template",
		},
		"AWS::SageMaker::ClusterSchedulerConfig": {
			"ClusterSchedulerConfigArn", "ClusterSchedulerConfigId",
			"arn:aws:sagemaker:us-east-1:123456789012:cluster-scheduler-config/abc123def456",
			"abc123def456",
		},
		"AWS::SageMaker::HumanTaskUi": {
			"HumanTaskUiArn", "HumanTaskUiName",
			"arn:aws:sagemaker:us-east-1:123456789012:human-task-ui/my-ui",
			"my-ui",
		},
		"AWS::SageMaker::Model": {
			"ModelArn", "ModelName",
			"arn:aws:sagemaker:us-east-1:123456789012:model/my-model",
			"my-model",
		},
		"AWS::SageMaker::NotebookInstanceLifecycleConfig": {
			"NotebookInstanceLifecycleConfigArn", "NotebookInstanceLifecycleConfigName",
			"arn:aws:sagemaker:us-east-1:123456789012:notebook-instance-lifecycle-config/my-config",
			"my-config",
		},
		"AWS::SageMaker::ProcessingJob": {
			"ProcessingJobArn", "ProcessingJobName",
			"arn:aws:sagemaker:us-east-1:123456789012:processing-job/my-job",
			"my-job",
		},
		"AWS::SageMaker::Project": {
			"ProjectArn", "ProjectName",
			"arn:aws:sagemaker:us-east-1:123456789012:project/my-project",
			"my-project",
		},
		"AWS::Shield::ProtectionGroup": {
			"ProtectionGroupArn", "ProtectionGroupId",
			"arn:aws:shield::123456789012:protection-group/my-group",
			"my-group",
		},
		"AWS::Signer::SigningProfile": {
			"Arn", "profileName",
			"arn:aws:signer:us-east-1:123456789012:/signing-profiles/MyProfile",
			"MyProfile",
		},
		"AWS::Transfer::Server": {
			"Arn", "ServerId",
			"arn:aws:transfer:us-east-1:123456789012:server/s-01234567890abcdef",
			"s-01234567890abcdef",
		},
		"AWS::WellArchitected::Workload": {
			"WorkloadArn", "WorkloadId",
			"arn:aws:wellarchitected:us-east-1:123456789012:workload/abcdef1234567890abcdef1234567890",
			"abcdef1234567890abcdef1234567890",
		},
		"AWS::Wickr::Network": {
			"NetworkArn", "networkId",
			"arn:aws:wickr:us-east-1:123456789012:network/12345678",
			"12345678",
		},
	} {
		t.Run(typeName, func(t *testing.T) {
			r, ok := readers[typeName]
			if !ok {
				t.Fatalf("no reader for %s", typeName)
			}

			want := "{" + c.arnProperty + ":arnName}"
			var got string
			var found bool
			for _, b := range r.Input {
				if b.Member == c.member {
					got, found = b.Value, true
				}
			}
			if !found {
				t.Fatalf("Input has no binding for %s; Input = %#v", c.member, r.Input)
			}
			if got != want {
				t.Fatalf("Input[%s].Value = %q, want %q", c.member, got, want)
			}

			if s := substitute(got, map[string]string{c.arnProperty: c.arn}); s != c.want {
				t.Fatalf("substitute(%q, %s=%q) = %q, want %q", got, c.arnProperty, c.arn, s, c.want)
			}

			var placeholder bool
			for _, b := range r.Identifier {
				if b.Property == c.arnProperty && b.Location == "placeholder" {
					placeholder = true
				}
			}
			if !placeholder {
				t.Fatalf("Identifier has no placeholder binding for %s; Identifier = %#v", c.arnProperty, r.Identifier)
			}
		})
	}
}
