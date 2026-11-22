<template>
    <div id="message-detail" class="detail-page">
        <Loader v-if="isLoading" label="Loading message" />

        <div v-else-if="isNotFound" class="card empty">
            <i class="fa-solid fa-envelope" aria-hidden="true" />
            <h2>Message not found</h2>
            <RouterLink class="btn" :to="{ name: 'app-messages', params: { appId: context.appId.value } }">Back to messages</RouterLink>
        </div>

        <ApiErrorNotice v-else-if="loadError" :error="loadError" :retry="load" />

        <template v-else-if="detail">
            <section class="card" aria-labelledby="message-heading">
                <div class="card-header">
                    <h2 id="message-heading" class="subject-title" data-fact="subject">
                        <span class="sr-only">Subject: </span>
                        <template v-if="detail.message.subject.kind === 'text'">
                            <span v-if="detail.message.subject.text" :title="detail.message.subject.text">{{ detail.message.subject.text }}</span>
                            <span v-else class="muted">(no subject)</span>
                        </template>
                        <span v-else-if="detail.message.subject.kind === 'protected'" class="muted">Protected. Shown with the message content.</span>
                        <span v-else class="muted">Subject not recorded</span>
                    </h2>
                    <span class="tag" :class="tagClass(messageStatus(detail.message.status).tone)" :title="messageStatus(detail.message.status).description">
                        {{ messageStatus(detail.message.status).label }}
                    </span>
                </div>

                <div class="card-body">
                    <dl class="kv">
                        <div data-fact="sender">
                            <dt>From</dt>
                            <dd v-if="detail.message.sender">
                                <span class="v" :title="senderText(detail.message.sender)">{{ senderText(detail.message.sender) }}</span>
                            </dd>
                            <dd v-else class="muted">Not recorded</dd>
                        </div>
                        <div v-if="!recipientGroups.length" data-fact="recipients">
                            <dt>To</dt>
                            <dd class="muted">Not recorded</dd>
                        </div>
                        <div v-for="group in recipientGroups" :key="group.kind" :data-fact="group.kind === 'to' ? 'recipients' : `recipients-${group.kind}`">
                            <dt :title="group.kind === 'bcc' ? 'Blind copies. These recipients are not listed in the message that anyone receives.' : undefined">{{ group.label }}</dt>
                            <dd>
                                <span class="v wrap">
                                    <span v-for="(address, i) in group.visible" :key="address.index" class="addr">{{ address.text }}<template v-if="i < group.visible.length - 1">,</template></span>
                                </span>
                                <button
                                    v-if="group.hidden > 0"
                                    type="button"
                                    class="link-btn more"
                                    :title="`Show all ${group.entries.length} recipients`"
                                    @click="showAllRecipients = true"
                                >
                                    +{{ group.hidden }} more<span class="sr-only">: show all {{ group.entries.length }}</span>
                                </button>
                                <button v-else-if="showAllRecipients && group.entries.length > RECIPIENTS_COLLAPSED" type="button" class="link-btn more" @click="showAllRecipients = false">
                                    Show fewer
                                </button>
                            </dd>
                        </div>
                        <div>
                            <dt title="When your app submitted the message to Thinpost">Submitted</dt>
                            <dd>
                                <time class="v" :datetime="detail.message.acceptedAt">{{ formatDateTime(detail.message.acceptedAt) }}</time>
                            </dd>
                        </div>
                        <div>
                            <dt>Message ID</dt>
                            <dd>
                                <code class="v id" :title="detail.message.id">{{ detail.message.id }}</code>
                                <button type="button" class="btn ghost sm icon" title="Copy message ID" @click="copyId(detail.message.id, 'message')">
                                    <i class="fa-solid fa-copy" aria-hidden="true" />
                                    <span class="sr-only">Copy message ID</span>
                                </button>
                            </dd>
                        </div>
                        <div data-fact="message-id-header">
                            <dt>Message-ID header</dt>
                            <dd v-if="messageIdHeader.kind === 'shared'">
                                <code class="v id" :title="messageIdHeader.value">{{ messageIdHeader.value }}</code>
                                <button type="button" class="btn ghost sm icon" title="Copy Message-ID header" @click="copyId(messageIdHeader.value, 'Message-ID header')">
                                    <i class="fa-solid fa-copy" aria-hidden="true" />
                                    <span class="sr-only">Copy Message-ID header</span>
                                </button>
                            </dd>
                            <dd v-else-if="messageIdHeader.kind === 'perRecipient'" class="muted" title="This message was sent before Thinpost used one Message-ID per message. Each recipient's copy has its own; open a recipient's Headers to see it.">Different for each recipient</dd>
                            <dd v-else-if="messageIdHeader.kind === 'pending'" class="muted" title="Assigned when Thinpost prepares the message for sending.">Not assigned yet</dd>
                            <dd v-else-if="messageIdHeader.kind === 'notSent'" class="muted" title="The message ended on hold without being sent.">None (not sent)</dd>
                            <dd v-else class="muted">Not recorded</dd>
                        </div>
                        <div data-fact="sender-domain">
                            <dt>Sender domain</dt>
                            <dd v-if="detail.message.senderDomain">
                                <span class="v" :title="detail.message.senderDomain">{{ detail.message.senderDomain }}</span>
                                <button
                                    type="button"
                                    class="btn ghost sm icon"
                                    title="Find messages from this domain"
                                    :disabled="authState.status !== 'signed-in'"
                                    @click="findSenderDomain(detail.message.senderDomain)"
                                >
                                    <i class="fa-solid fa-magnifying-glass" aria-hidden="true" />
                                    <span class="sr-only">Find messages from this domain</span>
                                </button>
                            </dd>
                            <dd v-else class="muted">Not recorded</dd>
                        </div>
                        <div v-if="detail.message.operatorTest" data-fact="console-test">
                            <dt>Console test</dt>
                            <dd>
                                <span class="tag info">Console test</span>
                                <span v-if="detail.message.operatorTest.bypassLimits" class="tag warning">limits skipped</span>
                                <span v-if="detail.message.operatorTest.bypassRecipientPolicy" class="tag warning">allowlist skipped</span>
                            </dd>
                        </div>
                        <div v-if="detail.message.smtp" data-fact="smtp">
                            <dt>Sent over SMTP</dt>
                            <dd>
                                <span class="tag info"><i class="fa-solid fa-paper-plane" aria-hidden="true" />SMTP</span>
                                <span class="v" :title="detail.message.smtp.credentialName">{{ detail.message.smtp.credentialName }}</span>
                            </dd>
                        </div>
                        <div v-if="detail.message.smtp?.originalFrom" data-fact="original-from">
                            <dt>Original From</dt>
                            <dd>
                                <span class="v" :title="detail.message.smtp.originalFrom">{{ detail.message.smtp.originalFrom }}</span>
                                <span class="muted">replaced by this app's From address setting</span>
                            </dd>
                        </div>
                        <div data-fact="enhanced-protection">
                            <dt>Enhanced Protection</dt>
                            <dd>
                                <span v-if="detail.message.enhancedProtection" class="tag info">
                                    <i class="fa-solid fa-shield-halved" aria-hidden="true" />On
                                </span>
                                <span v-else class="muted">Off</span>
                                <HelpButton topic="Enhanced Protection">
                                    <p data-testid="ep-hint">
                                        Your app chooses this for each message when it sends it. When on, Thinpost only delivers over an encrypted (TLS)
                                        connection whose certificate matches the mail server's name. That name comes from DNS lookups that aren't
                                        authenticated, so this confirms the server matches the name, not that it is the recipient's intended mail server.
                                    </p>
                                    <p>
                                        It protects only the connection to that mail server. Later hops may not use TLS, so this is not end-to-end
                                        encryption and doesn't by itself make you HIPAA compliant. This console doesn't show the encryption result of
                                        individual delivery attempts.
                                    </p>
                                    <p v-if="detail.message.enhancedProtection" data-testid="ep-audit-hint">
                                        The subject is hidden in lists and can't be searched. Revealing the subject, viewing the content,
                                        downloading attachments and viewing delivery headers are each recorded in the audit log under your name.
                                    </p>
                                </HelpButton>
                            </dd>
                        </div>
                        <div v-if="detail.hold" data-fact="hold">
                            <dt title="Your app submitted this message on hold. Only the app can release or cancel it.">Hold</dt>
                            <dd v-if="detail.hold.resolved">
                                {{ HOLD_RESOLUTIONS[detail.hold.resolved.resolution] }}
                                <time class="v" :datetime="detail.hold.resolved.time">{{ formatDateTime(detail.hold.resolved.time) }}</time>
                            </dd>
                            <dd v-else-if="detail.message.state === 'recovery_hold'" class="muted" title="A data recovery quarantined it while it was on hold. Your app can no longer release or cancel it, and it doesn't expire.">
                                Quarantined after data recovery
                            </dd>
                            <dd v-else>
                                Until released, or expires <time class="v" :datetime="detail.hold.expiresAt">{{ formatDateTime(detail.hold.expiresAt) }}</time>
                            </dd>
                        </div>
                        <div v-if="detail.message.state !== 'prepared'" data-fact="preparation">
                            <dt>Preparation</dt>
                            <dd :title="messageState(detail.message.state).description">
                                <span>{{ messageState(detail.message.state).label }}</span>
                                <code v-if="messageState(detail.message.state).label.toLowerCase() !== detail.message.state" class="raw">{{ detail.message.state }}</code>
                            </dd>
                        </div>
                    </dl>
                </div>
            </section>

            <section aria-labelledby="deliveries-heading" class="card">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="deliveries-heading">
                            Deliveries
                        </h2>
                        <HelpButton topic="deliveries">
                            <p>One delivery per recipient, including Cc and Bcc recipients. Accepted means the recipient's mail server took the message. It doesn't guarantee it reached the inbox. The number is the recipient's position in the original recipient list.</p>
                            <template v-if="hasProgress">
                                <p data-testid="attempt-hint">
                                    Attempts counts every try, including one in progress. Holds caused by recipient rules aren't counted.
                                    Next attempt is the earliest time a queued delivery can be tried, not when it will actually be sent. A paused app, closed mail intake, no connected sending worker or
                                    unavailable tracking can delay it.
                                </p>
                                <p>
                                    Last result is the most recent finished attempt, including outcomes Thinpost reached itself, such as a domain that
                                    doesn't exist. An SMTP code (the recipient mail server's reply) appears only if one was recorded. The reply text isn't shown.
                                </p>
                            </template>
                            <p v-else-if="detail.deliveries.length" data-testid="attempt-hint">Attempt counts and timing aren't available for this message.</p>
                        </HelpButton>
                        <ul class="summary" aria-label="Delivery counts">
                            <li v-for="part in summaryParts" :key="part.text" :class="part.tone">{{ part.text }}</li>
                        </ul>
                    </div>
                    <button type="button" class="btn sm" :disabled="isRefreshing || isRetrying || isAbandoning" @click="refresh">
                        <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isRefreshing }" aria-hidden="true" />
                        {{ isRefreshing ? 'Refreshing…' : 'Refresh' }}
                    </button>
                </div>

                <div v-if="heldCount > 0 || trackingHeldCount > 0 || actionError || refreshError" class="card-body deliveries-body">
                    <div v-if="heldCount > 0" class="notice warning">
                        <i class="fa-solid fa-hand" aria-hidden="true" />
                        <div class="notice-body">
                            <strong>{{ heldCount === 1 ? '1 delivery' : `${heldCount} deliveries` }} held by recipient rules.</strong>
                            <HelpButton topic="recipient rule holds">
                                <p>
                                    The recipient rules set by your Thinpost administrator don't allow this recipient, so the delivery is on hold.
                                    Held deliveries aren't retried automatically.
                                </p>
                                <p>
                                    Retrying puts the delivery back in the queue and checks the rules again. If the recipient is still not allowed, it goes
                                    back on hold. A retry can't override the rules.
                                </p>
                            </HelpButton>
                        </div>
                    </div>

                    <div v-if="trackingHeldCount > 0" class="notice warning">
                        <i class="fa-solid fa-link-slash" aria-hidden="true" />
                        <div class="notice-body">
                            <strong>{{ trackingHeldCount === 1 ? '1 delivery' : `${trackingHeldCount} deliveries` }} held: tracking unavailable.</strong>
                            <HelpButton topic="tracking holds">
                                <p>
                                    The tracking links prepared for this recipient were revoked, retired or about to expire. These deliveries aren't
                                    released or sent automatically.
                                </p>
                                <p>
                                    The only option is to mark the delivery failed. That doesn't send the message, and it doesn't apply to
                                    deliveries with an uncertain result.
                                </p>
                            </HelpButton>
                        </div>
                    </div>

                    <ApiErrorNotice
                        v-if="actionError"
                        :error="actionError"
                        :retry="reloadRequired ? refresh : undefined"
                        :retry-label="reloadRequired ? 'Reload deliveries' : undefined"
                    />
                    <ApiErrorNotice v-if="refreshError" :error="refreshError" :retry="refresh" />
                </div>

                <div v-if="!detail.deliveries.length" class="card-body empty compact">
                    <i class="fa-solid fa-paper-plane" aria-hidden="true" />
                    <h2>No deliveries yet</h2>
                </div>

                <div v-else class="table-wrap" :aria-busy="isRefreshing">
                    <table class="data deliveries">
                        <caption class="sr-only">Deliveries for message {{ detail.message.id }}</caption>
                        <thead>
                            <tr>
                                <th scope="col">Recipient</th>
                                <th scope="col">Status</th>
                                <template v-if="hasProgress">
                                    <th scope="col" class="num">Attempts</th>
                                    <th scope="col">Last result</th>
                                    <th scope="col">Next attempt</th>
                                </template>
                                <th v-if="hasEngagement" scope="col" class="engagement-col">
                                    Engagement
                                    <HelpButton topic="engagement">
                                        <p>Opens and clicks recorded for tracked deliveries. Per-link and daily limits apply, so counts can be lower than real activity.</p>
                                        <p>Email scanners and privacy proxies can trigger opens and clicks too. Unique opens, rates and link destinations aren't available.</p>
                                    </HelpButton>
                                </th>
                                <th scope="col" class="actions"><span class="sr-only">Actions</span></th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="delivery in detail.deliveries" :key="delivery.id" :data-delivery="delivery.id">
                                <td class="recipient-cell">
                                    <span v-if="detail.deliveries.length > 1" class="idx">#{{ delivery.recipient_index + 1 }}</span>
                                    <span
                                        v-if="readDeliveryRecipientKind(delivery) === 'cc' || readDeliveryRecipientKind(delivery) === 'bcc'"
                                        class="tag kind-tag"
                                        data-field="recipient-kind"
                                        :title="readDeliveryRecipientKind(delivery) === 'cc' ? 'This recipient was copied (Cc)' : 'This recipient was blind copied (Bcc)'"
                                    >
                                        {{ readDeliveryRecipientKind(delivery) === 'cc' ? 'Cc' : 'Bcc' }}
                                    </span>
                                    <span v-if="readDeliveryRecipient(delivery)" class="trunc" :title="readDeliveryRecipient(delivery) ?? undefined">{{ readDeliveryRecipient(delivery) }}</span>
                                    <span v-else class="muted">Not recorded</span>
                                </td>
                                <td>
                                    <span class="tag" :class="tagClass(deliveryStatus(delivery).tone)" :title="deliveryStatus(delivery).description">
                                        {{ deliveryStatus(delivery).label }}
                                    </span>
                                    <span
                                        v-for="tag in readDeliveryFeedback(delivery)"
                                        :key="tag.kind"
                                        class="tag feedback-tag"
                                        :class="tagClass(tag.tone)"
                                        :title="`${tag.label} ${formatDateTime(tag.receivedAt)}`"
                                        data-field="feedback"
                                    >
                                        {{ tag.label }}
                                    </span>
                                    <div v-if="readUnsubscribeForward(delivery)" class="forward-note" :class="`tone-${readUnsubscribeForward(delivery)!.tone}`" :title="readUnsubscribeForward(delivery)!.title" data-field="unsubscribe-forward">
                                        {{ readUnsubscribeForward(delivery)!.text }}
                                    </div>
                                </td>
                                <template v-if="hasProgress">
                                    <td class="num" data-field="retry-attempts">
                                        <template v-if="progressOf(delivery).retryAttempts !== null">{{ progressOf(delivery).retryAttempts }}</template>
                                        <span v-else class="muted">Not reported</span>
                                    </td>
                                    <!-- Current control planes always send retry_attempts; without it, absence of an attempt says nothing. -->
                                    <td data-field="last-attempt" class="nowrap">
                                        <template v-if="progressOf(delivery).hasCompletedAttempt">
                                            <span :title="lastResultTitle(progressOf(delivery))">{{ lastResultText(progressOf(delivery)) }}</span>
                                        </template>
                                        <span v-else-if="progressOf(delivery).retryAttempts === null" class="muted">Not reported</span>
                                        <span v-else class="muted">None completed</span>
                                    </td>
                                    <td data-field="next-attempt" class="nowrap">
                                        <template v-if="delivery.state === QUEUED">
                                            <span v-if="progressOf(delivery).nextAttemptAt" :title="`${NEXT_ATTEMPT_DESCRIPTION} Not before ${formatDateTime(progressOf(delivery).nextAttemptAt!)}.`">
                                                <time :datetime="progressOf(delivery).nextAttemptAt!">{{ formatRelativeTime(progressOf(delivery).nextAttemptAt!, now) }}</time>
                                            </span>
                                            <span v-else class="muted">Not reported</span>
                                        </template>
                                        <template v-else>
                                            <span class="muted" aria-hidden="true">—</span>
                                            <span class="sr-only">Not queued</span>
                                        </template>
                                    </td>
                                </template>
                                <td v-if="hasEngagement" class="nowrap" data-field="engagement">
                                    <template v-if="engagementOf(delivery)">
                                        Opens {{ engagementOf(delivery)!.opens }} · Clicks {{ engagementOf(delivery)!.clicks }}
                                        <template v-if="engagementOf(delivery)!.last_event_at">· last <time :datetime="engagementOf(delivery)!.last_event_at">{{ formatDateTime(engagementOf(delivery)!.last_event_at!) }}</time></template>
                                        <template v-else>· none yet</template>
                                    </template>
                                    <template v-else>
                                        <span class="muted" aria-hidden="true">—</span>
                                        <span class="sr-only">Not tracked</span>
                                    </template>
                                </td>
                                <td class="actions">
                                    <div class="action-row">
                                        <code class="did" :title="delivery.id">{{ shortId(delivery.id) }}</code>
                                        <button
                                            type="button"
                                            class="btn ghost sm icon"
                                            title="Copy delivery ID"
                                            :aria-label="`Copy delivery ID for recipient #${delivery.recipient_index + 1}`"
                                            @click="copyId(delivery.id, 'delivery')"
                                        >
                                            <i class="fa-solid fa-copy" aria-hidden="true" />
                                        </button>
                                        <button type="button" class="btn ghost sm" :disabled="!canView" @click="openHeaders(delivery)">
                                            <i class="fa-solid fa-file-lines" aria-hidden="true" />Headers<span class="sr-only"> for recipient #{{ delivery.recipient_index + 1 }}</span>
                                        </button>
                                        <DropdownMenu
                                            v-if="suppressionAction(delivery)"
                                            :label="`More actions for recipient #${delivery.recipient_index + 1}`"
                                            trigger-class="btn ghost sm icon"
                                        >
                                            <template #trigger>
                                                <i class="fa-solid fa-ellipsis" aria-hidden="true" />
                                            </template>
                                            <button
                                                v-if="suppressionAction(delivery) === 'find'"
                                                class="menu-item"
                                                type="button"
                                                role="menuitem"
                                                :aria-disabled="!canView ? 'true' : undefined"
                                                @click="findInSuppressions(delivery)"
                                            >
                                                <i class="fa-solid fa-ban" aria-hidden="true" />
                                                Find in suppression list
                                            </button>
                                            <button
                                                v-else
                                                class="menu-item"
                                                type="button"
                                                role="menuitem"
                                                :aria-disabled="!canAct || isSuppressing ? 'true' : undefined"
                                                @click="suppressRecipient(delivery)"
                                            >
                                                <i class="fa-solid fa-ban" aria-hidden="true" />
                                                Add to suppression list
                                            </button>
                                        </DropdownMenu>
                                        <template v-if="delivery.state === HELD">
                                            <button class="btn sm"
                                                type="button"
                                                :disabled="!!retryBlocker || !canAct"
                                                :aria-describedby="retryBlocker ? `${id}-blocker-${delivery.id}` : undefined"
                                                @click="openRetry(delivery)"
                                            >
                                                Retry
                                                <span class="sr-only">for recipient #{{ delivery.recipient_index + 1 }}</span>
                                            </button>
                                        </template>
                                        <template v-else-if="delivery.state === TRACKING_HELD">
                                            <button class="btn sm danger"
                                                type="button"
                                                :disabled="!!abandonBlocker || !canAct"
                                                :aria-describedby="abandonBlocker ? `${id}-blocker-${delivery.id}` : undefined"
                                                @click="openAbandon(delivery)"
                                            >
                                                Mark failed
                                                <span class="sr-only">without sending, for recipient #{{ delivery.recipient_index + 1 }}</span>
                                            </button>
                                        </template>
                                    </div>
                                    <p v-if="delivery.state === HELD && retryBlocker" :id="`${id}-blocker-${delivery.id}`" class="blocker">{{ retryBlocker }}</p>
                                    <p v-else-if="delivery.state === TRACKING_HELD && abandonBlocker" :id="`${id}-blocker-${delivery.id}`" class="blocker">{{ abandonBlocker }}</p>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </section>

            <PolicyHoldModal
                v-if="retryTarget"
                :app-id="app.id"
                :app-name="app.name"
                :message-id="detail.message.id"
                :delivery-id="retryTarget.id"
                :recipient-index="retryTarget.recipient_index"
                :recipient="readDeliveryRecipient(retryTarget)"
                :is-submitting="isRetrying"
                @close="closeRetry"
                @submit="confirmRetry"
            />

            <TrackingHoldModal
                v-if="abandonTarget"
                :app-id="app.id"
                :app-name="app.name"
                :app-active="app.active"
                :message-id="detail.message.id"
                :delivery-id="abandonTarget.id"
                :recipient-index="abandonTarget.recipient_index"
                :recipient="readDeliveryRecipient(abandonTarget)"
                :is-submitting="isAbandoning"
                @close="closeAbandon"
                @submit="confirmAbandon"
            />

            <DeliveryHeadersModal
                v-if="headersTarget"
                :app-id="app.id"
                :message-id="detail.message.id"
                :delivery-id="headersTarget.id"
                :recipient-label="recipientLabel(headersTarget)"
                :recipient-kind="readDeliveryRecipientKind(headersTarget)"
                :enhanced-protection="detail.message.enhancedProtection"
                @close="headersTarget = null"
            />

            <MessageContent
                :key="detail.message.id"
                :app-id="app.id"
                :message-id="detail.message.id"
                :enhanced-protection="detail.message.enhancedProtection"
            />
        </template>
    </div>
</template>

<script lang="ts" setup>
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef, useId, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription, type DescribeOptions } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { attemptOutcome, hasDeliveryProgress, readDeliveryProgress, smtpCodeLabel, type DeliveryProgress } from '@/delivery-progress';
import { confirmProtectedAccess } from '@/enhanced-protection';
import { readDeliveryFeedback, readUnsubscribeForward } from '@/feedback';
import { readMessageIdHeader } from '@/message-id-header';
import { copyText, formatDateTime, formatRelativeTime, shortId } from '@/format';
import { readDeliveryTracking } from '@/tracking';
import { saveSearch } from '@/message-search-store';
import { saveSuppressionSearch } from '@/suppression-search-store';
import { readDeliveryRecipient, readDeliveryRecipientKind, readMessageHold, readMessageSummary, type HoldView, type MessageRow, type RecipientKind } from '@/message-summary';
import { DefaultApi, type DeliverySummary } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import DropdownMenu from '@/shared/components/dropdown-menu.vue';
import HelpButton from '@/shared/components/help-button.vue';
import DeliveryHeadersModal from '@/shared/components/delivery-headers-modal.vue';
import Loader from '@/shared/components/loader.vue';
import MessageContent from '@/shared/components/message-content.vue';
import PolicyHoldModal from '@/shared/components/policy-hold-modal.vue';
import TrackingHoldModal from '@/shared/components/tracking-hold-modal.vue';
import { deliveryState, messageState, messageStatus, tagClass } from '@/states';
import { isOpaqueId } from '@/validation';

const HELD = 'recipient_policy_held';
const TRACKING_HELD = 'tracking_held';
const QUEUED = 'queued';

const NEXT_ATTEMPT_DESCRIPTION =
    'Earliest time Thinpost will try again. The actual attempt may be later.';

const UNKNOWN_OUTCOME_REASON = 'an earlier action may or may not have gone through.';

const ABANDON_ERRORS: DescribeOptions = {
    mutation: true,
    notFound:
        'Nothing was marked failed. That delivery wasn\'t found for this message.',
    conflict:
        'Nothing was marked failed. The delivery is no longer held because tracking is unavailable (it may already have been marked failed).',
    unavailable:
        'Marking this delivery failed is temporarily unavailable. Try again in a moment.'
};

const RETRY_ERRORS: DescribeOptions = {
    mutation: true,
    notFound: 'That delivery wasn\'t found for this message. Nothing was requeued.',
    conflict:
        'Nothing was requeued. The delivery is no longer held by recipient rules (it may already have been retried), or the app is paused. The deliveries have been reloaded.',
    unavailable: 'Retrying is temporarily unavailable. Try again in a moment.'
};

const route = useRoute();
const router = useRouter();
const id = useId();
const context = useAppContext();
const { app } = context;

interface Detail {
    message: MessageRow;
    deliveries: DeliverySummary[];
    /** Null unless the app submitted the message on hold. */
    hold: HoldView | null;
}

const RECIPIENTS_COLLAPSED = 10;

const HOLD_RESOLUTIONS = { released: 'Released', cancelled: 'Cancelled', expired: 'Expired' } as const;

const detail = shallowRef<Detail | null>(null);
const isLoading = ref(true);
const isNotFound = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const isRefreshing = ref(false);
const refreshError = ref<ApiErrorDescription | null>(null);

const retryTarget = shallowRef<DeliverySummary | null>(null);
const isRetrying = ref(false);
const abandonTarget = shallowRef<DeliverySummary | null>(null);
const isAbandoning = ref(false);

const actionError = ref<ApiErrorDescription | null>(null);
/**
 * Set when the deliveries on screen cannot be trusted for another action, for example after a response
 * with an unknown outcome. Delivery actions stay blocked until the deliveries have been reloaded.
 */
const reloadRequired = ref<string | null>(null);

const headersTarget = shallowRef<DeliverySummary | null>(null);
const showAllRecipients = ref(false);

const RECIPIENT_GROUPS: { kind: RecipientKind; label: string }[] = [
    { kind: 'to', label: 'To' },
    { kind: 'cc', label: 'Cc' },
    { kind: 'bcc', label: 'Bcc' }
];
/** To, Cc and Bcc rows; a row appears only when it has recipients. */
const recipientGroups = computed(() => {
    const recipients = detail.value?.message.recipients;
    if (recipients?.kind !== 'list') return [];
    return RECIPIENT_GROUPS.map(({ kind, label }) => {
        const entries = recipients.shown.map((text, index) => ({ text, index })).filter(entry => recipients.kinds[entry.index] === kind);
        const visible = showAllRecipients.value ? entries : entries.slice(0, RECIPIENTS_COLLAPSED);
        return { kind, label, entries, visible, hidden: entries.length - visible.length };
    }).filter(group => group.entries.length > 0);
});

const messageIdHeader = computed(() => readMessageIdHeader(detail.value?.message.id ?? '', detail.value?.message.state ?? '', detail.value?.deliveries ?? []));
const heldCount = computed(() => detail.value?.deliveries.filter(d => d.state === HELD).length ?? 0);
const trackingHeldCount = computed(() => detail.value?.deliveries.filter(d => d.state === TRACKING_HELD).length ?? 0);
/** Control planes deployed before the per-delivery attempt fields send none of them; the columns are then omitted. */
const hasProgress = computed(() => detail.value?.deliveries.some(hasDeliveryProgress) ?? false);
/** Per-delivery engagement exists only for tracked deliveries; older control planes send none. */
const engagementById = computed(() => new Map((detail.value?.deliveries ?? []).map(d => [d.id, readDeliveryTracking(d)])));
const hasEngagement = computed(() => [...engagementById.value.values()].some(Boolean));
const engagementOf = (delivery: DeliverySummary) => engagementById.value.get(delivery.id) ?? null;
const progressById = computed(() => new Map((detail.value?.deliveries ?? []).map(d => [d.id, readDeliveryProgress(d)])));
/** Non-zero counts only, for the card heading. */
const summaryParts = computed(() => {
    const m = detail.value?.message;
    if (!m) return [];
    const parts: Array<{ text: string; tone: string }> = [];
    const add = (count: number, text: string, tone = '') => {
        if (count > 0) parts.push({ text: `${count} ${text}`, tone });
    };
    add(m.deliveryCount, m.deliveryCount === 1 ? 'recipient' : 'recipients');
    add(m.acceptedCount, 'accepted');
    add(m.failedCount, 'failed', 'danger');
    add(m.uncertainCount, 'uncertain', 'warning');
    add(heldCount.value, 'held by rules', 'warning');
    add(trackingHeldCount.value, 'held (tracking)', 'warning');
    return parts;
});
const NO_PROGRESS = readDeliveryProgress(undefined);

function progressOf(delivery: DeliverySummary): DeliveryProgress {
    return progressById.value.get(delivery.id) ?? NO_PROGRESS;
}
const canAct = computed(
    () => authState.status === 'signed-in' && !isRetrying.value && !isAbandoning.value && !isRefreshing.value && !isLoading.value
);
const canView = computed(() => authState.status === 'signed-in' && !isLoading.value);
/** Why retry is not offered, from what this page knows; the server re-checks everything. */
const retryBlocker = computed(() => {
    if (reloadRequired.value) return `Reload the deliveries first: ${reloadRequired.value}`;
    if (app.value && !app.value.active) return 'The app is paused.';
    return null;
});
/** Inactive apps can still have tracking holds marked failed; their webhook delivery waits for resumption. */
const abandonBlocker = computed(() => (reloadRequired.value ? `Reload the deliveries first: ${reloadRequired.value}` : null));

let loadSeq = 0;
let isUnmounted = false;

// Separate getters so query or hash navigation does not reload (and re-audit) the message.
watch([() => context.appId.value, () => route.params.messageId], load, { immediate: true });
// Relative times re-render as the clock advances.
const now = ref(Date.now());
const clock = setInterval(() => (now.value = Date.now()), 30_000);
onBeforeUnmount(() => {
    clearInterval(clock);
    // The confirm dialog is global and can outlive this page; this stops a later confirmation from opening a modal.
    isUnmounted = true;
});

async function load() {
    const appId = context.appId.value;
    const messageId = route.params.messageId;
    const seq = ++loadSeq;
    isLoading.value = true;
    isNotFound.value = false;
    loadError.value = null;
    isRefreshing.value = false;
    refreshError.value = null;
    actionError.value = null;
    retryTarget.value = null;
    abandonTarget.value = null;
    headersTarget.value = null;
    showAllRecipients.value = false;
    // An action still in flight for the previous route is ignored; this load shows the current states.
    isRetrying.value = false;
    isAbandoning.value = false;
    detail.value = null;

    if (!isOpaqueId(messageId)) {
        isNotFound.value = true;
        isLoading.value = false;
        return;
    }

    try {
        const data = await fetchDetail(appId, messageId);
        if (seq !== loadSeq) return;
        detail.value = data;
        // Fresh delivery states now show whether an action with an unknown outcome took effect.
        reloadRequired.value = null;
    } catch (err) {
        if (seq !== loadSeq) return;
        if (apiErrorStatus(err) === 404) isNotFound.value = true;
        else loadError.value = describeApiError(err);
    } finally {
        if (seq === loadSeq) isLoading.value = false;
    }
}

async function fetchDetail(appId: string, messageId: string): Promise<Detail> {
    const response = await DefaultApi.getAppMessage({ path: { appId, messageId } });
    const data = response.data;
    const message = readMessageSummary(data?.message, { maxRecipients: Infinity });
    const hold = readMessageHold((data as { hold?: unknown } | undefined)?.hold);
    if (!message || !Array.isArray(data?.deliveries) || hold === undefined) throw new InvalidResponseError();
    return { message, deliveries: data.deliveries, hold };
}

/**
 * Reloads the app and the delivery states while keeping the current rows, and any source the
 * operator chose to view, on screen. Does not change loadSeq: it is the same message.
 * Resolves true only when fresh delivery states are on screen.
 */
async function refresh(): Promise<boolean> {
    const appId = app.value?.id;
    const messageId = detail.value?.message.id;
    if (!appId || !messageId || isRefreshing.value) return false;
    const seq = loadSeq;
    isRefreshing.value = true;
    refreshError.value = null;
    try {
        const [found, data] = await Promise.all([context.reload(), fetchDetail(appId, messageId)]);
        if (seq !== loadSeq || isUnmounted) return false;
        if (!found) {
            refreshError.value = { kind: 'not-found', message: 'The app was not found. The deliveries below are from the last time they loaded.' };
            return false;
        }
        detail.value = data;
        if (reloadRequired.value) {
            reloadRequired.value = null;
            actionError.value = null;
        }
        return true;
    } catch (err) {
        if (seq !== loadSeq || isUnmounted) return false;
        const description = describeApiError(err, { notFound: 'That message wasn\'t found.' });
        refreshError.value = { ...description, message: `${description.message} The deliveries below are from the last time they loaded.` };
        return false;
    } finally {
        if (seq === loadSeq) isRefreshing.value = false;
    }
}

function openRetry(delivery: DeliverySummary) {
    if (!canAct.value || retryBlocker.value || delivery.state !== HELD) return;
    actionError.value = null;
    retryTarget.value = delivery;
}

function closeRetry() {
    if (isRetrying.value) return;
    retryTarget.value = null;
}

async function confirmRetry() {
    const target = retryTarget.value;
    const appId = app.value?.id;
    const messageId = detail.value?.message.id;
    // The session may have ended, or the deliveries reloaded, while the dialog was open.
    const current = detail.value?.deliveries.find(d => d.id === target?.id);
    if (!target || !appId || !messageId || !canAct.value || retryBlocker.value || current?.state !== HELD) {
        retryTarget.value = null;
        return;
    }
    const seq = loadSeq;
    const isCurrent = () => !isUnmounted && seq === loadSeq;
    isRetrying.value = true;
    actionError.value = null;
    let reload = false;
    try {
        const response = await DefaultApi.retryDeliveryPolicyHold({ path: { appId, messageId, deliveryId: target.id } });
        if (!isCurrent()) return;
        if (response?.response?.status !== 204) throw new InvalidResponseError();
        showToast({
            message: 'Delivery requeued and the recipient rules will be checked again. It goes back on hold if the recipient is still not allowed.',
            durationSecs: 6
        });
        reload = true;
    } catch (err) {
        if (!isCurrent()) return;
        const status = apiErrorStatus(err);
        actionError.value = describeApiError(err, RETRY_ERRORS);
        if (status === 404 || status === 409) {
            // Definitive refusal; the delivery or the app changed under us.
            reload = true;
        } else if (status === undefined || status >= 500 || err instanceof InvalidResponseError) {
            // No definitive answer: the requeue may have been committed. Block repeats until reloaded.
            reloadRequired.value = UNKNOWN_OUTCOME_REASON;
        }
    } finally {
        if (seq === loadSeq) {
            isRetrying.value = false;
            retryTarget.value = null;
        }
    }
    if (reload && isCurrent()) await refresh();
}

function openAbandon(delivery: DeliverySummary) {
    if (!canAct.value || abandonBlocker.value || delivery.state !== TRACKING_HELD) return;
    actionError.value = null;
    abandonTarget.value = delivery;
}

function closeAbandon() {
    if (isAbandoning.value) return;
    abandonTarget.value = null;
}

async function confirmAbandon() {
    const target = abandonTarget.value;
    const appId = app.value?.id;
    const messageId = detail.value?.message.id;
    // The session may have ended, or the deliveries reloaded, while the dialog was open.
    const current = detail.value?.deliveries.find(d => d.id === target?.id);
    if (!target || !appId || !messageId || !canAct.value || abandonBlocker.value || current?.state !== TRACKING_HELD) {
        abandonTarget.value = null;
        return;
    }
    const seq = loadSeq;
    const isCurrent = () => !isUnmounted && seq === loadSeq;
    isAbandoning.value = true;
    actionError.value = null;
    let outcome: 'failed' | 'refused' | 'unknown' | 'marked' = 'failed';
    let description: ApiErrorDescription | null = null;
    try {
        const response = await DefaultApi.abandonDeliveryTrackingHold({ path: { appId, messageId, deliveryId: target.id } });
        if (!isCurrent()) return;
        if (response?.response?.status !== 204) throw new InvalidResponseError();
        outcome = 'marked';
    } catch (err) {
        if (!isCurrent()) return;
        const status = apiErrorStatus(err);
        description = describeApiError(err, ABANDON_ERRORS);
        // Only these responses are definitive refusals; anything else (no response, 5xx, an unexpected
        // status or body) may have followed a commit.
        if (status === 404 || status === 409) outcome = 'refused';
        else if (status === 400 || status === 401 || status === 403) outcome = 'failed';
        else outcome = 'unknown';
    } finally {
        if (seq === loadSeq) {
            isAbandoning.value = false;
            abandonTarget.value = null;
        }
    }

    if (outcome === 'unknown') {
        // No definitive answer: the delivery may have been marked failed. Block repeats until reloaded.
        actionError.value = description;
        reloadRequired.value = UNKNOWN_OUTCOME_REASON;
        return;
    }
    if (outcome === 'failed') {
        // A definitive rejection of the request itself (for example 400 or 403); nothing changed.
        actionError.value = description;
        return;
    }

    if (outcome === 'marked') {
        showToast({
            message: 'Delivery marked failed without sending it. This was recorded in the audit log.',
            durationSecs: 6
        });
    }
    // A definitive answer means the delivery on screen is out of date either way; show its current state.
    const reloaded = await refresh();
    if (!isCurrent()) return;
    if (!reloaded) reloadRequired.value = 'the deliveries shown may be out of date.';
    if (description) {
        const next = reloaded
            ? 'The deliveries below have been reloaded. Check their current state before trying again.'
            : 'Reload the deliveries to check their current state before trying again.';
        actionError.value = { ...description, message: `${description.message} ${next}` };
    }
}

async function openHeaders(delivery: DeliverySummary) {
    const current = detail.value;
    if (!current || !canView.value) return;
    const seq = loadSeq;
    const confirmed = await confirmProtectedAccess('headers', { enhancedProtection: current.message.enhancedProtection });
    if (!confirmed || isUnmounted || seq !== loadSeq || authState.status !== 'signed-in') return;
    headersTarget.value = delivery;
}

function recipientLabel(delivery: DeliverySummary): string {
    const address = readDeliveryRecipient(delivery);
    return address ? `#${delivery.recipient_index + 1} ${address}` : `#${delivery.recipient_index + 1}`;
}

const isSuppressing = ref(false);

/** Which suppression action applies to a delivery; the recipient address must be known. */
/** A delivery dropped because the recipient was suppressed ends failed, but is shown as Suppressed. */
function deliveryStatus(delivery: DeliverySummary) {
    if (delivery.state === 'failed' && progressOf(delivery).lastOutcomeKey === 'suppressed') return attemptOutcome('suppressed');
    return deliveryState(delivery.state);
}

function suppressionAction(delivery: DeliverySummary): 'find' | 'add' | null {
    if (!readDeliveryRecipient(delivery)) return null;
    if (progressOf(delivery).lastOutcomeKey === 'suppressed') return 'find';
    if (delivery.state === 'failed') return 'add';
    return null;
}

function findInSuppressions(delivery: DeliverySummary) {
    const address = readDeliveryRecipient(delivery);
    if (!address || !canView.value) return;
    // Handed over in memory only; never in the URL or browser storage.
    saveSuppressionSearch(context.appId.value, address);
    void router.push({ name: 'app-suppressions', params: { appId: context.appId.value } });
}

async function suppressRecipient(delivery: DeliverySummary) {
    const address = readDeliveryRecipient(delivery);
    const appId = app.value?.id;
    if (!address || !appId || !canAct.value || isSuppressing.value || suppressionAction(delivery) !== 'add') return;
    const seq = loadSeq;
    const confirmed = await showConfirm('Add to suppression list?', `Thinpost will stop sending to ${address} for this app.`);
    if (!confirmed || isUnmounted || seq !== loadSeq || !canAct.value) return;
    isSuppressing.value = true;
    actionError.value = null;
    try {
        const response = await DefaultApi.addAppSuppressions({ path: { appId }, body: { addresses: [address] } });
        const results = response.data?.results;
        if (!Array.isArray(results) || results.length !== 1) throw new InvalidResponseError();
        if (!isUnmounted) showToast({ message: results[0]!.status === 'added' ? 'Added to suppression list' : 'Already on the suppression list', durationSecs: 3 });
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        actionError.value = describeApiError(err, {
            mutation: true,
            notFound: 'The app was not found.',
            unavailable: 'The suppression list is temporarily unavailable. Try again in a moment.'
        });
    } finally {
        isSuppressing.value = false;
    }
}

function findSenderDomain(domain: string) {
    saveSearch(context.appId.value, { q: domain, field: 'sender_domain' });
    void router.push({ name: 'app-messages', params: { appId: context.appId.value } });
}

async function copyId(value: string, kind: 'message' | 'delivery' | 'Message-ID header') {
    const ok = await copyText(value);
    showToast({ message: ok ? `Copied ${kind === 'Message-ID header' ? kind : `${kind} ID`}` : 'Couldn\'t copy', durationSecs: 2 });
}

function senderText(sender: { name?: string | null; address: string }): string {
    return sender.name ? `${sender.name} <${sender.address}>` : sender.address;
}

const SHORT_OUTCOMES: Record<string, string> = {
    accepted: 'Accepted',
    rejected: 'Rejected',
    route_rejected: 'One server rejected',
    uncertain: 'Unknown'
};

/** Compact last-result cell: SMTP code with a short outcome when both are recorded, otherwise whichever exists. */
function lastResultText(progress: DeliveryProgress): string {
    const code = progress.lastSmtpCode;
    const key = progress.lastOutcomeKey;
    const hasCode = code !== null && code > 0;
    if (hasCode && key === 'accepted') return `Accepted · ${code}`;
    if (hasCode && (key === 'rejected' || (!progress.lastOutcome && code >= 400))) {
        if (code >= 400 && code <= 499) return `${code} · temporary`;
        if (code >= 500 && code <= 599) return `${code} · permanent`;
    }
    if (progress.lastOutcome) {
        const short = key ? (SHORT_OUTCOMES[key] ?? progress.lastOutcome.label) : progress.lastOutcome.label;
        return hasCode ? `${short} · ${code}` : progress.lastOutcome.label;
    }
    if (hasCode) return `${code}`;
    return 'Outcome not reported';
}

function lastResultTitle(progress: DeliveryProgress): string {
    const parts: string[] = [];
    if (progress.lastOutcome) parts.push(`${progress.lastOutcome.label}. ${progress.lastOutcome.description}`);
    if (progress.lastSmtpCode !== null) {
        const code = smtpCodeLabel(progress.lastSmtpCode);
        parts.push(`${code.text}. ${code.description}`);
    } else {
        parts.push('No SMTP code was recorded.');
    }
    return parts.join(' ');
}
</script>

<style lang="scss" scoped>
.detail-page {
    display: flex;
    flex-direction: column;
    gap: 16px;
}

.subject-title {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
}

.kv {
    display: grid;
    grid-template-columns: 140px minmax(0, 1fr);
    gap: 8px 16px;
    margin: 0;
    font-size: 13px;

    > div {
        display: contents;
    }

    dt {
        color: var(--text-3);
        white-space: nowrap;
    }

    dd {
        display: flex;
        align-items: center;
        gap: 6px;
        min-width: 0;
        margin: 0;
    }

    .v {
        min-width: 0;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;

        &.wrap {
            white-space: normal;
        }
    }

    .addr {
        display: inline-block;
        max-width: 100%;
        margin-right: 0.35em;
        overflow-wrap: anywhere;
    }

    .id {
        font-size: 12.5px;
    }

    .more {
        flex: none;
        white-space: nowrap;
    }

    .btn.icon.sm {
        flex: none;
        height: 22px;
    }
}

.raw {
    color: var(--text-3);
}

.deliveries-body {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

.heading-with-help {
    flex: 1;
    flex-wrap: wrap;
    row-gap: 2px;
}

.summary {
    display: flex;
    gap: 0;
    min-width: 0;
    flex-wrap: wrap;
    margin: 0 0 0 4px;
    padding: 0;
    color: var(--text-3);
    font-size: 12.5px;
    list-style: none;

    li + li::before {
        content: '·';
        margin: 0 6px;
    }

    .danger {
        color: var(--danger);
    }

    .warning {
        color: var(--warning);
    }
}

.empty.compact {
    min-height: 160px;
}

.deliveries {
    .feedback-tag {
        margin-left: 6px;
    }

    .forward-note {
        margin-top: 4px;
        color: var(--text-3);
        font-size: 11.5px;

        &.tone-danger {
            color: var(--danger, var(--text-3));
        }
    }

    .did {
        margin-right: 4px;
        color: var(--text-3);
        font-size: 11.5px;
    }

    // The actions column takes the slack, so the recipient column sizes to its
    // content; the fixed cap (not a percentage) keeps long addresses from squeezing it.
    .recipient-cell {
        white-space: nowrap;

        .idx {
            margin-right: 8px;
            color: var(--text-3);
            font-variant-numeric: tabular-nums;
        }

        .kind-tag {
            margin-right: 8px;
        }

        .trunc {
            display: inline-block;
            max-width: 360px;
            overflow: hidden;
            text-overflow: ellipsis;
            vertical-align: bottom;
        }

    }

    .nowrap {
        white-space: nowrap;
    }

    // Takes the slack so the data columns sit together and the actions stay at the right edge.
    .actions {
        width: 100%;
        text-align: right;
        white-space: nowrap;
    }
}

.action-row {
    display: inline-flex;
    align-items: center;
    justify-content: flex-end;
    gap: 4px;
}

.blocker {
    max-width: 14rem;
    margin: 4px 0 0 auto;
    white-space: normal;
}

@media (max-width: 640px) {
    .kv {
        grid-template-columns: minmax(0, 1fr);
        gap: 0;

        dt {
            margin-top: 10px;
            font-size: 12px;
        }
    }
}
</style>
