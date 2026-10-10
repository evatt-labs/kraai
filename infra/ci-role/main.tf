# The role the live workflow (.github/workflows/live.yml) assumes through
# GitHub's OIDC provider, which already exists in the account. Applied by
# hand by an account administrator; nothing in CI changes it.
#
# Trust: only a workflow run from main of evatt-labs/kraai, scheduled or
# dispatched; a dispatch from any other branch is refused.
#
# Permissions: the actions kraai's own iam-policy says the two live
# fixtures need (actions.json, regenerated with `make ci-role-actions`), on
# every resource, since a handler publishes actions and not the identifiers
# the provider assigns; except IAM, scoped to the roles the live
# environments derive (kci-*), with managed policy attachments limited to
# AWS's service-role policies. A Deny then confines every destructive verb
# of the services that address a resource by name to the kci-* names the
# live environments derive, and the lock bucket's objects: whatever the
# Allow grants, the workflow cannot delete or overwrite anything else. EC2
# resources are addressed by id and are not confined, nor is deregistering
# a task definition, which ECS authorizes only on every resource; a
# condition on kraai's identity tag would be the way to. The role can still put an
# inline policy on a kci-* role and run a function with it, so what it can
# do is bounded by the code merged to main, not by this policy alone.
terraform {
  required_version = ">= 1.8"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}

provider "aws" {
  region = "us-east-1"
}

variable "subject_prefix" {
  description = <<-EOT
    The repository whose main branch may assume the role, as its OIDC token's
    subject names it. The repository uses immutable subjects, which carry the
    owner's and the repository's ids, so a repository deleted and made again
    under the same name cannot assume the role. Read it with
    `gh api repos/<owner>/<repo>/actions/oidc/customization/sub`.
  EOT
  type        = string
  default     = "repo:evatt-labs@227429383/kraai@1329760535"
}

data "aws_caller_identity" "current" {}

data "aws_iam_openid_connect_provider" "github" {
  url = "https://token.actions.githubusercontent.com"
}

locals {
  actions      = jsondecode(file("${path.module}/actions.json"))
  iam_actions  = [for a in local.actions : a if startswith(a, "iam:")]
  rest_actions = [for a in local.actions : a if !startswith(a, "iam:")]
  # What OpenTofu reads and writes to make the fixture's VPC, beside what
  # kraai itself needs.
  tofu_actions = [
    "ec2:CreateTags",
    "ec2:DescribeAccountAttributes",
    "ec2:DescribeAvailabilityZones",
    "ec2:DescribeNetworkAcls",
    "ec2:DescribeRouteTables",
    "ec2:DescribeSecurityGroups",
    "ec2:DescribeTags",
  ]
  account    = data.aws_caller_identity.current.account_id
  live_roles = "arn:aws:iam::${local.account}:role/kci-*"
  # A role holds at most 10,240 bytes of inline policy, which the actions
  # exceed, so they are split across managed policies, each well under the
  # 6,144 characters one may hold.
  action_chunks = chunklist(sort(distinct(concat(local.rest_actions, local.tofu_actions))), 120)
}

data "aws_iam_policy_document" "trust" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [data.aws_iam_openid_connect_provider.github.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["${var.subject_prefix}:ref:refs/heads/main"]
    }
  }
}

data "aws_iam_policy_document" "scoped" {
  statement {
    sid       = "ListRoles"
    actions   = ["iam:ListRoles"]
    resources = ["*"]
  }
  statement {
    sid       = "LiveEnvironmentRoles"
    actions   = [for a in local.iam_actions : a if !contains(["iam:ListRoles", "iam:CreateServiceLinkedRole", "iam:AttachRolePolicy"], a)]
    resources = [local.live_roles]
  }
  statement {
    sid       = "ServiceRolePolicies"
    actions   = ["iam:AttachRolePolicy"]
    resources = [local.live_roles]
    condition {
      test     = "ArnLike"
      variable = "iam:PolicyARN"
      values   = ["arn:aws:iam::aws:policy/service-role/*"]
    }
  }
  statement {
    sid       = "ServiceLinkedRoles"
    actions   = ["iam:CreateServiceLinkedRole"]
    resources = ["arn:aws:iam::*:role/aws-service-role/*"]
  }
  statement {
    sid    = "OnlyLiveResourcesAreDestroyed"
    effect = "Deny"
    actions = [
      "cloudwatch:DeleteAlarms",
      "dynamodb:DeleteTable",
      "ecs:DeleteCluster",
      "elasticache:DeleteCacheSubnetGroup",
      "elasticloadbalancing:DeleteTargetGroup",
      "events:DeleteEventBus",
      "events:DeleteRule",
      "events:RemoveTargets",
      "lambda:DeleteFunction",
      "lambda:DeleteFunctionUrlConfig",
      "lambda:RemovePermission",
      "logs:DeleteLogGroup",
      "rds:DeleteDBClusterParameterGroup",
      "rds:DeleteDBSubnetGroup",
      "s3:DeleteBucket",
      "s3:DeleteObject",
      "s3:DeleteObjectVersion",
      "sqs:DeleteQueue",
      "sqs:PurgeQueue",
      "ssm:DeleteParameter",
      "ssm:DeleteParameters",
      "ssm:PutParameter",
    ]
    not_resources = [
      "arn:aws:cloudwatch:*:${local.account}:alarm:kci-*",
      "arn:aws:dynamodb:*:${local.account}:table/kci-*",
      "arn:aws:ecs:*:${local.account}:cluster/kci-*",
      "arn:aws:elasticache:*:${local.account}:subnetgroup:kci-*",
      "arn:aws:elasticloadbalancing:*:${local.account}:targetgroup/kci-*",
      "arn:aws:events:*:${local.account}:event-bus/kci-*",
      "arn:aws:events:*:${local.account}:rule/kci-*",
      "arn:aws:lambda:*:${local.account}:function:kci-*",
      "arn:aws:logs:*:${local.account}:log-group:kci-*",
      "arn:aws:rds:*:${local.account}:cluster-pg:kci-*",
      "arn:aws:rds:*:${local.account}:subgrp:kci-*",
      "arn:aws:s3:::kci-*",
      "arn:aws:s3:::kci-*/*",
      "arn:aws:s3:::kraai-lock-${local.account}-*/*",
      "arn:aws:sqs:*:${local.account}:kci-*",
      "arn:aws:ssm:*:${local.account}:parameter/kci-*",
    ]
  }
}

resource "aws_iam_role" "live" {
  name                 = "kraai-live-ci"
  description          = "Assumed by kraai's live workflow on main to apply and destroy its live fixtures."
  assume_role_policy   = data.aws_iam_policy_document.trust.json
  max_session_duration = 7200
}

data "aws_iam_policy_document" "actions" {
  count = length(local.action_chunks)
  statement {
    sid       = "Kraai${count.index}"
    actions   = local.action_chunks[count.index]
    resources = ["*"]
  }
}

resource "aws_iam_policy" "actions" {
  count       = length(local.action_chunks)
  name        = "kraai-live-ci-actions-${count.index}"
  description = "Part ${count.index} of the actions kraai's live fixtures need."
  policy      = data.aws_iam_policy_document.actions[count.index].json
}

resource "aws_iam_policy" "scoped" {
  name        = "kraai-live-ci-scoped"
  description = "IAM confined to the live environments' roles, and deletes confined to their kci-* names."
  policy      = data.aws_iam_policy_document.scoped.json
}

resource "aws_iam_role_policy_attachment" "actions" {
  count      = length(aws_iam_policy.actions)
  role       = aws_iam_role.live.name
  policy_arn = aws_iam_policy.actions[count.index].arn
}

resource "aws_iam_role_policy_attachment" "scoped" {
  role       = aws_iam_role.live.name
  policy_arn = aws_iam_policy.scoped.arn
}

output "role_arn" {
  description = "Set as the repository variable KRAAI_LIVE_ROLE_ARN."
  value       = aws_iam_role.live.arn
}
