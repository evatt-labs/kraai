# The VPC the live workflow's environment sits on, as Terraform would own
# it in a real account. Two subnets in two zones; no NAT, gateway or
# endpoint, so it costs nothing. State stays on the runner.
terraform {
  required_version = ">= 1.8"
  backend "local" {}
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}

variable "name" {
  description = "The environment's name, which tags what this makes."
  type        = string
}

provider "aws" {
  region = "us-east-1"
  default_tags {
    tags = { "kraai:live-ci" = var.name }
  }
}

# No flow logs: the VPC lives for one workflow run and carries no traffic
# beyond the function's interfaces being made and released, and a flow log
# would add a log group and its cost to every run. An explicit exception,
# owned by the live workflow.
#trivy:ignore:AWS-0178
resource "aws_vpc" "this" {
  cidr_block = "10.88.0.0/16"
}

resource "aws_subnet" "a" {
  vpc_id            = aws_vpc.this.id
  cidr_block        = "10.88.1.0/24"
  availability_zone = "us-east-1a"
}

resource "aws_subnet" "b" {
  vpc_id            = aws_vpc.this.id
  cidr_block        = "10.88.2.0/24"
  availability_zone = "us-east-1b"
}

output "vpc_id" {
  value = aws_vpc.this.id
}

output "subnet_ids" {
  value = [aws_subnet.a.id, aws_subnet.b.id]
}
