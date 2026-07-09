import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, within } from "storybook/test";
import { MockUserMember } from "#/testHelpers/entities";
import { withAuthProvider } from "#/testHelpers/storybook";
import AccessPage from "./AccessPage";

const meta: Meta<typeof AccessPage> = {
	title: "pages/AccessPage",
	component: AccessPage,
	decorators: [withAuthProvider],
	parameters: {
		user: MockUserMember,
	},
};

export default meta;
type Story = StoryObj<typeof AccessPage>;

export const Default: Story = {
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByRole("heading", { name: /access restricted/i }),
		).toBeInTheDocument();
		await expect(
			canvas.getByRole("button", { name: /return to platform/i }),
		).toBeInTheDocument();
		await expect(
			canvas.queryByRole("button", { name: /sign out/i }),
		).not.toBeInTheDocument();
	},
};
