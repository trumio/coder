import type { FC } from "react";
import { Button } from "#/components/Button/Button";
import { ProductLogo } from "#/components/Icons/ProductLogo";
import { getApplicationName } from "#/utils/appearance";

const AccessPage: FC = () => {
	const applicationName = getApplicationName();

	// Full-page navigation to the server-side redirect, which knows the
	// configured platform URL. Kept out of the SPA so it stays per-environment.
	const goToPlatform = () => {
		location.href = "/platform-login";
	};

	return (
		<>
			<title>{`Access restricted - ${applicationName}`}</title>
			<div className="p-6 flex items-center justify-center min-h-full text-center">
				<div className="w-full max-w-sm flex flex-col items-center gap-4">
					<ProductLogo />
					<h1 className="text-xl font-semibold m-0">Access restricted</h1>
					<p className="text-sm text-content-secondary m-0">
						Your account does not have access to the {applicationName}{" "}
						dashboard. Open your workspace from the platform instead.
					</p>
					<Button size="lg" className="w-full" onClick={goToPlatform}>
						Return to platform
					</Button>
				</div>
			</div>
		</>
	);
};

export default AccessPage;
