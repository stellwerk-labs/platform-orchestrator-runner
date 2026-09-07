terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.9.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "3.2.1"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "6.63.0"
    }
    faulty = {
      source  = "astromechza/faulty"
      version = "0.1.0"
    }
  }
}
