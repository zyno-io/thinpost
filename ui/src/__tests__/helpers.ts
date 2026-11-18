import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { OpenApiError } from '@zyno-io/openapi-client-codegen';

/** An error shaped like the generated client's failure for an HTTP response. */
export function httpError(status: number): OpenApiError {
    const response = new Response(JSON.stringify({ error: 'x' }), { status });
    return new OpenApiError(`x (${status})`, new Request('http://localhost/api/v1/x'), response, { error: 'x' });
}

/** An error shaped like the generated client's failure when no response arrives. */
export function networkError(): OpenApiError {
    const cause = new TypeError('Failed to fetch');
    return new OpenApiError('TypeError: Failed to fetch', new Request('http://localhost/api/v1/x'), undefined as unknown as Response, undefined, cause);
}

/** Opens a help modal through its "About <topic>" button and returns the dialog text. */
export async function openHelp(wrapper: VueWrapper, topic: string): Promise<string> {
    const button = wrapper.find(`button[aria-label="About ${topic}"]`);
    if (!button.exists()) throw new Error(`No help button for ${topic}`);
    await button.trigger('click');
    await flushPromises();
    const dialogs = document.querySelectorAll('[role="dialog"]');
    const dialog = dialogs[dialogs.length - 1];
    if (!dialog) throw new Error(`Help for ${topic} did not open a dialog`);
    return dialog.textContent ?? '';
}
