package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"testing"

	tfjson "github.com/hashicorp/terraform-json"
	"github.com/stretchr/testify/require"
)

func diskPlan(t *testing.T) map[string]any {
	t.Helper()
	var plan map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"format_version":"1.2","terraform_version":"1.13.2","planned_values":{"root_module":{"resources":[{"address":"aws_ebs_volume.disk","mode":"managed","type":"aws_ebs_volume","name":"disk","provider_name":"registry.terraform.io/hashicorp/aws","values":{"type":"gp3","size":100,"iops":3000,"throughput":125,"availability_zone":"eu-central-1a","id":null,"arn":null,"secret":"SECRET_COST_CANARY"},"sensitive_values":{"secret":true}}]}},"resource_changes":[{"address":"aws_ebs_volume.disk","mode":"managed","type":"aws_ebs_volume","name":"disk","provider_name":"registry.terraform.io/hashicorp/aws","change":{"actions":["create"],"after_unknown":{"id":true,"arn":true},"after_sensitive":{"kms_key_id":true}}}],"configuration":{"root_module":{}}}`), &plan))
	return plan
}
func costFixtureResource(plan map[string]any) map[string]any {
	return plan["planned_values"].(map[string]any)["root_module"].(map[string]any)["resources"].([]any)[0].(map[string]any)
}
func costFixtureChange(plan map[string]any) map[string]any {
	return plan["resource_changes"].([]any)[0].(map[string]any)["change"].(map[string]any)
}
func estimateFixture(t *testing.T, plan map[string]any) (diskEstimate, error) {
	t.Helper()
	data, err := json.Marshal(plan)
	require.NoError(t, err)
	return estimateDisks(data)
}

func TestCostRealCataloguePlannedCoverage(t *testing.T) {
	for _, action := range []string{"create", "update", "no-op", "unchanged", "nested"} {
		t.Run(action, func(t *testing.T) {
			plan := diskPlan(t)
			if action == "unchanged" {
				delete(plan, "resource_changes")
			} else if action == "nested" {
				resource := costFixtureResource(plan)
				resource["address"] = `module.storage["prod"].aws_ebs_volume.disk[0]`
				plan["resource_changes"].([]any)[0].(map[string]any)["address"] = resource["address"]
				root := plan["planned_values"].(map[string]any)["root_module"].(map[string]any)
				root["child_modules"] = []any{map[string]any{"resources": root["resources"]}}
				delete(root, "resources")
			} else {
				costFixtureChange(plan)["actions"] = []string{action}
			}
			data, err := json.Marshal(plan)
			require.NoError(t, err)
			var parsed tfjson.Plan
			require.NoError(t, json.Unmarshal(data, &parsed), "fixture must also satisfy real Terraform JSON parser")
			estimate, err := estimateDisks(data)
			require.NoError(t, err)
			require.Equal(t, 1, estimate.Priced)
			require.InDelta(t, 9.52, estimate.Monthly, 0.000001)
			require.Equal(t, 1, estimate.Resources)
		})
	}
}
func TestCostConsumedFieldsAndMasks(t *testing.T) {
	for _, field := range []string{"type", "size", "iops", "throughput"} {
		for _, kind := range []string{"missing", "unknown", "sensitive", "planned_sensitive", "parent_unknown", "parent_sensitive", "primitive", "array"} {
			t.Run(field+"/"+kind, func(t *testing.T) {
				plan := diskPlan(t)
				resource := costFixtureResource(plan)
				change := costFixtureChange(plan)
				switch kind {
				case "missing":
					delete(resource["values"].(map[string]any), field)
				case "unknown":
					change["after_unknown"] = map[string]any{field: true}
				case "sensitive":
					change["after_sensitive"] = map[string]any{field: true}
				case "planned_sensitive":
					resource["sensitive_values"] = map[string]any{field: true}
				case "parent_unknown":
					change["after_unknown"] = true
				case "parent_sensitive":
					resource["sensitive_values"] = true
				case "primitive":
					change["after_unknown"] = 42
				case "array":
					change["after_sensitive"] = map[string]any{field: []any{false, true}}
				}
				estimate, err := estimateFixture(t, plan)
				require.NoError(t, err)
				require.Zero(t, estimate.Priced)
				require.Equal(t, 1, estimate.Unknown)
				require.Zero(t, estimate.Monthly)
			})
		}
	}
}
func azureDiskPlan(t *testing.T, storage string) map[string]any {
	plan := diskPlan(t)
	resource := costFixtureResource(plan)
	resource["address"] = "azurerm_managed_disk.disk"
	resource["type"] = "azurerm_managed_disk"
	resource["provider_name"] = "registry.terraform.io/hashicorp/azurerm"
	resource["values"] = map[string]any{"storage_account_type": storage, "disk_size_gb": 100, "location": "West Europe", "tier": nil, "on_demand_bursting_enabled": false, "max_shares": 1}
	delete(plan, "resource_changes")
	return plan
}
func TestCostAzureCatalogue(t *testing.T) {
	for _, storage := range []string{"Premium_LRS", "Premium_ZRS"} {
		t.Run(storage, func(t *testing.T) {
			estimate, err := estimateFixture(t, azureDiskPlan(t, storage))
			require.NoError(t, err)
			require.Equal(t, 1, estimate.Priced)
			require.InDelta(t, map[string]float64{"Premium_LRS": 21.68, "Premium_ZRS": 32.519}[storage], estimate.Monthly, 0.000001)
			require.False(t, math.IsNaN(estimate.Monthly))
		})
	}
}
func TestCostAzureOmittedChargeGuards(t *testing.T) {
	for _, field := range []string{"tier", "on_demand_bursting_enabled", "max_shares"} {
		for _, kind := range []string{"nondefault", "after_unknown", "after_sensitive"} {
			t.Run(field+"/"+kind, func(t *testing.T) {
				plan := azureDiskPlan(t, "Premium_LRS")
				r := costFixtureResource(plan)
				if kind == "nondefault" {
					r["values"].(map[string]any)[field] = map[string]any{"tier": "P50", "on_demand_bursting_enabled": true, "max_shares": 2}[field]
				} else {
					c := map[string]any{"actions": []string{"create"}, kind: map[string]any{field: true}}
					plan["resource_changes"] = []any{map[string]any{"address": r["address"], "mode": r["mode"], "type": r["type"], "name": r["name"], "provider_name": r["provider_name"], "change": c}}
				}
				estimate, err := estimateFixture(t, plan)
				require.NoError(t, err)
				require.Zero(t, estimate.Priced)
				require.Equal(t, 1, estimate.Unknown)
			})
		}
	}
}
func TestCostRegionResolution(t *testing.T) {
	for _, kind := range []string{"alias_literal", "variable", "unknown", "sensitive_variable", "wrong_region", "missing", "unknown_zone"} {
		t.Run(kind, func(t *testing.T) {
			checkCostRegionCase(t, kind)
		})
	}
}
func checkCostRegionCase(t *testing.T, kind string) {

	plan := diskPlan(t)
	resource := costFixtureResource(plan)
	values := resource["values"].(map[string]any)
	delete(values, "availability_zone")
	expression := map[string]any{"constant_value": "eu-central-1"}
	if kind == "variable" || kind == "sensitive_variable" {
		expression = map[string]any{"references": []string{"var.region"}}
		plan["variables"] = map[string]any{"region": map[string]any{"value": "eu-central-1"}}
	}
	if kind == "unknown" {
		expression = map[string]any{"references": []string{"local.region"}}
	}
	if kind == "wrong_region" {
		expression["constant_value"] = "wrong-region"
	}
	plan["configuration"] = map[string]any{"provider_config": map[string]any{"aws.europe": map[string]any{"name": "aws", "expressions": map[string]any{"region": expression}}}, "root_module": map[string]any{"resources": []any{map[string]any{"address": "aws_ebs_volume.disk", "provider_config_key": "aws.europe"}}, "variables": map[string]any{"region": map[string]any{"sensitive": kind == "sensitive_variable"}}}}
	if kind == "missing" || kind == "unknown_zone" {
		delete(plan, "configuration")
	}
	if kind == "unknown_zone" {
		values["availability_zone"] = "eu-central-1a"
		costFixtureChange(plan)["after_unknown"] = map[string]any{"availability_zone": true}
	}
	estimate, err := estimateFixture(t, plan)
	require.NoError(t, err)
	if kind == "alias_literal" || kind == "variable" {
		require.Equal(t, 1, estimate.Priced)
	} else {
		require.Zero(t, estimate.Priced)
		require.Equal(t, 1, estimate.Unpriced+estimate.Unknown)
	}
}

func TestCostCoverageNeverSilentlyFree(t *testing.T) {
	for _, kind := range []string{"google_compute_disk", "othercloud_vm", "aws_instance", "aws_vpc", "azurerm_resource_group"} {
		t.Run(kind, func(t *testing.T) {
			plan := diskPlan(t)
			r := costFixtureResource(plan)
			r["address"] = kind + ".disk"
			r["type"] = kind
			delete(plan, "resource_changes")
			if strings.HasPrefix(kind, "azurerm_") {
				r["provider_name"] = "registry.terraform.io/hashicorp/azurerm"
			}
			estimate, err := estimateFixture(t, plan)
			require.NoError(t, err)
			if kind == "aws_vpc" || kind == "azurerm_resource_group" {
				require.Equal(t, 1, estimate.Free)
			} else {
				require.Equal(t, 1, estimate.Unpriced)
				require.Zero(t, estimate.Free)
			}
		})
	}
}
func TestCostRejectsMalformedAmbiguousAndBoundedPlans(t *testing.T) {
	for _, kind := range []string{"duplicate_resource", "duplicate_change", "identity", "wrong_mode", "deleted_planned", "resource_limit", "bad_type"} {
		t.Run(kind, func(t *testing.T) {
			plan := diskPlan(t)
			r := costFixtureResource(plan)
			root := plan["planned_values"].(map[string]any)["root_module"].(map[string]any)
			switch kind {
			case "duplicate_resource":
				root["resources"] = []any{r, r}
			case "duplicate_change":
				plan["resource_changes"] = append(plan["resource_changes"].([]any), plan["resource_changes"].([]any)[0])
			case "identity":
				r["name"] = "other"
			case "wrong_mode":
				r["mode"] = "data"
			case "deleted_planned":
				costFixtureChange(plan)["actions"] = []string{"delete"}
			case "resource_limit":
				resources := make([]any, maxCostResources+1)
				for i := range resources {
					resources[i] = r
				}
				root["resources"] = resources
			case "bad_type":
				r["values"] = []any{1}
			}
			_, err := estimateFixture(t, plan)
			require.Error(t, err)
		})
	}
	for _, data := range [][]byte{[]byte(`{"format_version":"1.2","format_version":"1.2"}`), []byte(`{`), []byte(`{} {}`), bytes.Repeat([]byte(" "), maxCostPlanBytes+1)} {
		_, err := estimateDisks(data)
		require.Error(t, err)
	}
}
func TestCostLogMoneyCoveragePrivacyAndOptOut(t *testing.T) {
	original := slog.Default()
	defer slog.SetDefault(original)
	for _, kind := range []string{"known", "partial", "all_unpriced", "free", "disabled", "failure"} {
		t.Run(kind, func(t *testing.T) {
			checkCostLogCase(t, kind)
		})
	}
}
func checkCostLogCase(t *testing.T, kind string) {

	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	plan := diskPlan(t)
	root := plan["planned_values"].(map[string]any)["root_module"].(map[string]any)
	switch kind {
	case "partial":
		root["resources"] = append(root["resources"].([]any), map[string]any{"address": "othercloud_vm.other", "name": "other", "type": "othercloud_vm", "mode": "managed", "provider_name": "SECRET_COST_CANARY"})
	case "all_unpriced":
		r := costFixtureResource(plan)
		r["type"] = "othercloud_vm"
		r["address"] = "othercloud_vm.disk"
		delete(plan, "resource_changes")
	case "free":
		r := costFixtureResource(plan)
		r["type"] = "aws_vpc"
		r["address"] = "aws_vpc.disk"
		delete(plan, "resource_changes")
	}
	data, err := json.Marshal(plan)
	require.NoError(t, err)
	if kind == "failure" {
		data = []byte("SECRET_COST_CANARY")
	}
	r := baseRunner{costEstimationDisabled: kind == "disabled"}
	r.logDiskEstimate(context.Background(), data)
	output := logs.String()
	require.NotContains(t, output, "SECRET_COST_CANARY")
	require.NotContains(t, output, "9.520000000")
	if kind == "disabled" {
		require.Empty(t, output)
		return
	}
	require.Equal(t, 1, strings.Count(output, "\n"))
	require.Less(t, len(output), 1500)
	require.Contains(t, output, `"catalogue_date":"2026-10-20"`)
	if kind == "known" || kind == "partial" {
		require.Contains(t, output, `"known_monthly_subtotal":"9.52"`)
	}
	if kind == "partial" {
		require.Contains(t, output, `"status":"partial"`)
	}
	if kind == "all_unpriced" || kind == "failure" {
		require.NotContains(t, output, "known_monthly_subtotal")
		require.Contains(t, output, `"status":"unknown"`)
	}
	if kind == "free" {
		require.Contains(t, output, `"known_monthly_subtotal":"0.00"`)
	}
}

func TestCostRefusesAmbiguousProviderAndChildVariables(t *testing.T) {
	for _, kind := range []string{"missing_provider", "wrong_provider", "child_variable", "conflicting_region"} {
		t.Run(kind, func(t *testing.T) {
			plan := diskPlan(t)
			resource := costFixtureResource(plan)
			values := resource["values"].(map[string]any)
			provider := map[string]any{"name": "aws", "expressions": map[string]any{"region": map[string]any{"constant_value": "eu-central-1"}}}
			providers := map[string]any{"aws.europe": provider}
			switch kind {
			case "missing_provider":
				delete(providers, "aws.europe")
			case "wrong_provider":
				provider["name"] = "othercloud"
			case "child_variable":
				delete(values, "availability_zone")
				provider["module_address"] = "module.child"
				provider["expressions"] = map[string]any{"region": map[string]any{"references": []string{"var.region"}}}
				plan["variables"] = map[string]any{"region": map[string]any{"value": "us-east-1"}}
			case "conflicting_region":
				values["availability_zone"] = "us-east-1a"
			}
			plan["configuration"] = map[string]any{"provider_config": providers, "root_module": map[string]any{"resources": []any{map[string]any{"address": "aws_ebs_volume.disk", "provider_config_key": "aws.europe"}}}}
			estimate, err := estimateFixture(t, plan)
			require.NoError(t, err)
			require.Zero(t, estimate.Priced)
			require.Equal(t, 1, estimate.Unknown)
		})
	}
}

func TestCostResourceRegionOverride(t *testing.T) {
	knownMonthly := map[string]float64{"absent": 9.52, "null": 9.52, "known_override": 8}
	for _, kind := range []string{"absent", "null", "known_override", "conflicting_zone", "unknown", "sensitive", "planned_sensitive", "wrong_type", "empty"} {
		t.Run(kind, func(t *testing.T) {
			plan := diskPlan(t)
			resource := costFixtureResource(plan)
			values := resource["values"].(map[string]any)
			delete(values, "availability_zone")
			plan["configuration"] = map[string]any{"provider_config": map[string]any{"aws": map[string]any{"name": "aws", "expressions": map[string]any{"region": map[string]any{"constant_value": "eu-central-1"}}}}, "root_module": map[string]any{"resources": []any{map[string]any{"address": "aws_ebs_volume.disk", "provider_config_key": "aws"}}}}
			switch kind {
			case "null":
				values["region"] = nil
			case "known_override", "conflicting_zone":
				values["region"] = "us-east-1"
				if kind == "conflicting_zone" {
					values["availability_zone"] = "eu-central-1a"
				}
			case "unknown":
				costFixtureChange(plan)["after_unknown"] = map[string]any{"region": true, "availability_zone": true}
			case "sensitive":
				values["region"] = "us-east-1"
				costFixtureChange(plan)["after_sensitive"] = map[string]any{"region": true}
			case "planned_sensitive":
				values["region"] = "us-east-1"
				resource["sensitive_values"] = map[string]any{"region": true}
			case "wrong_type":
				values["region"] = 42
			case "empty":
				values["region"] = ""
			}
			estimate, err := estimateFixture(t, plan)
			require.NoError(t, err)
			if monthly, known := knownMonthly[kind]; known {
				require.Equal(t, 1, estimate.Priced)
				require.InDelta(t, monthly, estimate.Monthly, 0.000001)
			} else {
				require.Zero(t, estimate.Priced)
				require.Equal(t, 1, estimate.Unknown)
			}
		})
	}
}

func TestCostDeletedResourcesAndMissingUsageStayOutsideSubtotal(t *testing.T) {
	plan := diskPlan(t)
	changes := plan["resource_changes"].([]any)
	changes = append(changes, map[string]any{"address": "aws_ebs_volume.deleted", "mode": "managed", "type": "aws_ebs_volume", "name": "deleted", "provider_name": "registry.terraform.io/hashicorp/aws", "change": map[string]any{"actions": []string{"delete"}}})
	plan["resource_changes"] = changes
	root := plan["planned_values"].(map[string]any)["root_module"].(map[string]any)
	root["resources"] = append(root["resources"].([]any), map[string]any{"address": "aws_s3_bucket.unmeasured", "mode": "managed", "type": "aws_s3_bucket", "name": "unmeasured", "provider_name": "registry.terraform.io/hashicorp/aws", "values": map[string]any{}})
	estimate, err := estimateFixture(t, plan)
	require.NoError(t, err)
	require.Equal(t, 2, estimate.Resources)
	require.Equal(t, 1, estimate.Priced)
	require.Equal(t, 1, estimate.Unpriced)
	require.InDelta(t, 9.52, estimate.Monthly, 0.000001)
}
