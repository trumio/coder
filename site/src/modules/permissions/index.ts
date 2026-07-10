import type { AuthorizationCheck } from "#/api/typesGenerated";
import permissionChecksData from "../../../permissions.json";

export type Permissions = {
	[k in PermissionName]: boolean;
};

type PermissionName = keyof typeof permissionChecks;

/**
 * Site-wide permission checks, loaded from the shared
 * permissions.json that is also used by the Go backend.
 */
export const permissionChecks =
	permissionChecksData as typeof permissionChecksData &
		Record<string, AuthorizationCheck>;

export const canViewDeploymentSettings = (
	permissions: Permissions | undefined,
): permissions is Permissions => {
	return (
		permissions !== undefined &&
		(permissions.viewDeploymentConfig ||
			permissions.viewAllLicenses ||
			permissions.viewAllUsers ||
			permissions.viewAnyGroup ||
			permissions.viewNotificationTemplate ||
			permissions.viewOrganizationIDPSyncSettings ||
			permissions.viewAnyAIProvider ||
			permissions.viewAIGatewayKeys)
	);
};

/**
 * Checks if the user can view or edit members or groups for the organization
 * that produced the given OrganizationPermissions.
 */
export const canViewAnyOrganization = (
	permissions: Permissions | undefined,
): permissions is Permissions => {
	return (
		permissions !== undefined &&
		(permissions.viewAnyMembers ||
			permissions.editAnyGroups ||
			permissions.assignAnyRoles ||
			permissions.viewAnyIdpSyncSettings ||
			permissions.editAnySettings)
	);
};

/**
 * Checks if the user has any administrative capability that grants access to
 * the dashboard. Users without any of these capabilities are redirected away
 * from dashboard pages.
 *
 * Note: this intentionally does NOT use canViewAnyOrganization, because that
 * helper treats viewAnyMembers as sufficient, and ordinary organization
 * members hold viewAnyMembers (they can see who else is in their org). The
 * genuine org-admin capabilities (editAnySettings, assignAnyRoles,
 * editAnyGroups, viewAnyIdpSyncSettings) are checked directly instead so that
 * a plain member is correctly denied dashboard access.
 */
export const canViewDashboard = (permissions: Permissions): boolean => {
	// Field accesses come before the canViewDeploymentSettings type-guard call:
	// that guard narrows `permissions` to `never` for any operands after it in
	// the || chain, which would break the trailing field accesses.
	return (
		permissions.viewAnyAuditLog ||
		permissions.viewAnyConnectionLog ||
		permissions.viewDebugInfo ||
		permissions.createTemplates ||
		permissions.updateTemplates ||
		permissions.editAnySettings ||
		permissions.assignAnyRoles ||
		permissions.editAnyGroups ||
		permissions.viewAnyIdpSyncSettings ||
		canViewDeploymentSettings(permissions)
	);
};
