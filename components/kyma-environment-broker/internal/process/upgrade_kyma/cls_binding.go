package upgrade_kyma

import (
	"fmt"
	"time"

	"github.com/kyma-project/control-plane/components/kyma-environment-broker/internal/cls"
	kebError "github.com/kyma-project/control-plane/components/kyma-environment-broker/internal/error"
	"github.com/kyma-project/control-plane/components/kyma-environment-broker/internal/runtime/components"
	"github.com/kyma-project/control-plane/components/provisioner/pkg/gqlschema"

	"github.com/google/uuid"
	"github.com/kyma-project/control-plane/components/kyma-environment-broker/internal"
	"github.com/kyma-project/control-plane/components/kyma-environment-broker/internal/process"
	"github.com/kyma-project/control-plane/components/kyma-environment-broker/internal/process/provisioning"
	"github.com/kyma-project/control-plane/components/kyma-environment-broker/internal/storage"
	"github.com/sirupsen/logrus"
)

const (
	kibanaURLLabelKey = "operator_lmsUrl"
)

type ClsUpgradeBindStep struct {
	config           *cls.Config
	operationManager *process.UpgradeKymaOperationManager
	secretKey        string
	bindingProvider  provisioning.ClsBindingProvider
}

func NewClsUpgradeBindStep(config *cls.Config, bp provisioning.ClsBindingProvider, os storage.Operations, secretKey string) *ClsUpgradeBindStep {
	return &ClsUpgradeBindStep{
		config:           config,
		operationManager: process.NewUpgradeKymaOperationManager(os),
		secretKey:        secretKey,
		bindingProvider:  bp,
	}
}

var _ Step = (*ClsUpgradeBindStep)(nil)

func (s *ClsUpgradeBindStep) Name() string {
	return "CLS_UpgradeBind"
}

func (s *ClsUpgradeBindStep) Run(operation internal.UpgradeKymaOperation, log logrus.FieldLogger) (internal.UpgradeKymaOperation, time.Duration, error) {
	if !operation.Cls.Instance.Provisioned {
		failureReason := "CLS instance was not provisioned"
		log.Error(failureReason)
		return s.operationManager.OperationFailed(operation, failureReason, log)
	}

	var overrideParams *cls.OverrideParams
	var err error
	if operation.Cls.Overrides == "" {
		smCredentials, err := cls.FindCredentials(s.config.ServiceManager, operation.Cls.Region)
		if err != nil {
			failureReason := fmt.Sprintf("Unable to find credentials for CLS Service Manager in region %s", operation.Cls.Region)
			log.Errorf("%s: %v", failureReason, err)
			return s.operationManager.OperationFailed(operation, failureReason, log)
		}
		smCli := operation.SMClientFactory.ForCredentials(smCredentials)

		if operation.Cls.BindingID == "" {
			op, retry := s.operationManager.UpdateOperation(operation, func(operation *internal.UpgradeKymaOperation) {
				operation.Cls.BindingID = uuid.New().String()
			}, log)
			if retry > 0 {
				log.Errorf("Unable to update operation")
				return operation, time.Second, nil
			}
			operation = op
		}

		overrideParams, err = s.bindingProvider.CreateBinding(smCli, &cls.BindingRequest{
			InstanceKey: operation.Cls.Instance.InstanceKey(),
			BindingID:   operation.Cls.BindingID,
		})
		if err != nil {
			failureReason := "Unable to create CLS Binding"
			log.Errorf("%s: %v", failureReason, err)
			if kebError.IsTemporaryError(err) {
				return s.operationManager.RetryOperation(operation, failureReason, 10*time.Second, time.Minute*30, log)
			}
			return s.operationManager.OperationFailed(operation, failureReason, log)
		}

		encryptedOverrideParams, err := cls.EncryptOverrides(s.secretKey, overrideParams)
		if err != nil {
			failureReason := "Unable to encrypt CLS overrides"
			log.Errorf("%s: %v", failureReason, err)
			return s.operationManager.OperationFailed(operation, failureReason, log)
		}

		op, retry := s.operationManager.UpdateOperation(operation, func(operation *internal.UpgradeKymaOperation) {
			operation.Cls.Overrides = encryptedOverrideParams
		}, log)
		if retry > 0 {
			log.Errorf("Unable to update operation")
			return operation, time.Second, nil
		}
		operation = op
	} else {
		overrideParams, err = cls.DecryptOverrides(s.secretKey, operation.Cls.Overrides)
		if err != nil {
			failureReason := "Unable to decrypt CLS overrides"
			log.Errorf("%s: %v", failureReason, err)
			return s.operationManager.OperationFailed(operation, failureReason, log)
		}
	}

	extraConfTemplate, err := cls.GetExtraConfTemplate()
	if err != nil {
		failureReason := "Unable to get CLS extra config template"
		log.Errorf("%s: %v", failureReason, err)
		return s.operationManager.OperationFailed(operation, failureReason, log)
	}

	fluentBitClsOverrides, err := cls.RenderOverrides(overrideParams, extraConfTemplate)
	if err != nil {
		failureReason := "Unable to render CLS overrides"
		log.Errorf("%s: %v", failureReason, err)
		return s.operationManager.OperationFailed(operation, failureReason, log)
	}

	// TODO: delete this check (isVersionAtLeast1_20) after all SKR clusters are migrated to 1.20!
	isVersionAtLeast1_20, err := cls.IsKymaVersionAtLeast_1_20(operation.RuntimeVersion.Version)
	if err != nil {
		failureReason := "Unable to check kyma version"
		log.Errorf("%s: %v", failureReason, err)
		return s.operationManager.OperationFailed(operation, failureReason, log)
	}
	if isVersionAtLeast1_20 {
		// Disable LMS and enable CLS
		operation.InputCreator.AppendOverrides(components.CLS, []*gqlschema.ConfigEntryInput{
			{Key: "fluent-bit.config.outputs.forward.enabled", Value: "false"},
			{Key: "fluent-bit.config.outputs.additional", Value: fluentBitClsOverrides},
		})
	}

	return operation, 0, nil
}
