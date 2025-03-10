package settings

import (
	"context"

	"github.com/databricks/databricks-sdk-go"
	"github.com/databricks/databricks-sdk-go/service/settings"
)

// Default Namespace Setting
var defaultNamespaceSetting = workspaceSetting[settings.DefaultNamespaceSetting]{
	settingStruct: settings.DefaultNamespaceSetting{},
	readFunc: func(ctx context.Context, w *databricks.WorkspaceClient, etag string) (*settings.DefaultNamespaceSetting, error) {
		return w.Settings.ReadDefaultWorkspaceNamespace(ctx, settings.ReadDefaultWorkspaceNamespaceRequest{
			Etag: etag,
		})
	},
	updateFunc: func(ctx context.Context, w *databricks.WorkspaceClient, t settings.DefaultNamespaceSetting) (string, error) {
		t.SettingName = "default"
		res, err := w.Settings.UpdateDefaultWorkspaceNamespace(ctx, settings.UpdateDefaultWorkspaceNamespaceRequest{
			AllowMissing: true,
			Setting:      &t,
			FieldMask:    "namespace.value",
		})
		if err != nil {
			return "", err
		}
		return res.Etag, err
	},
	deleteFunc: func(ctx context.Context, w *databricks.WorkspaceClient, etag string) (string, error) {
		res, err := w.Settings.DeleteDefaultWorkspaceNamespace(ctx, settings.DeleteDefaultWorkspaceNamespaceRequest{
			Etag: etag,
		})
		if err != nil {
			return "", err
		}
		return res.Etag, err
	},
}
