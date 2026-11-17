// Hangup causes: the class of each cause (ok, unanswered, failed), a short
// label and a plain-language explanation. The keys are literal so the
// catalog test can see them; a cause not listed here is "failed" and is
// labelled with its own name.

import { t } from './i18n.js';

const CAUSES = {
  NORMAL_CLEARING: { cls: 'ok', label: () => t('cause.NORMAL_CLEARING.label'), text: () => t('cause.NORMAL_CLEARING.text') },
  ATTENDED_TRANSFER: { cls: 'ok', label: () => t('cause.ATTENDED_TRANSFER.label'), text: () => t('cause.ATTENDED_TRANSFER.text') },
  BLIND_TRANSFER: { cls: 'ok', label: () => t('cause.BLIND_TRANSFER.label'), text: () => t('cause.BLIND_TRANSFER.text') },
  LOSE_RACE: { cls: 'ok', label: () => t('cause.LOSE_RACE.label'), text: () => t('cause.LOSE_RACE.text') },
  PICKED_OFF: { cls: 'ok', label: () => t('cause.PICKED_OFF.label'), text: () => t('cause.PICKED_OFF.text') },
  ORIGINATOR_CANCEL: { cls: 'unanswered', label: () => t('cause.ORIGINATOR_CANCEL.label'), text: () => t('cause.ORIGINATOR_CANCEL.text') },
  NO_ANSWER: { cls: 'unanswered', label: () => t('cause.NO_ANSWER.label'), text: () => t('cause.NO_ANSWER.text') },
  NO_USER_RESPONSE: { cls: 'unanswered', label: () => t('cause.NO_USER_RESPONSE.label'), text: () => t('cause.NO_USER_RESPONSE.text') },
  USER_BUSY: { cls: 'unanswered', label: () => t('cause.USER_BUSY.label'), text: () => t('cause.USER_BUSY.text') },
  CALL_REJECTED: { cls: 'unanswered', label: () => t('cause.CALL_REJECTED.label'), text: () => t('cause.CALL_REJECTED.text') },
  ALLOTTED_TIMEOUT: { cls: 'unanswered', label: () => t('cause.ALLOTTED_TIMEOUT.label'), text: () => t('cause.ALLOTTED_TIMEOUT.text') },
  PROGRESS_TIMEOUT: { cls: 'unanswered', label: () => t('cause.PROGRESS_TIMEOUT.label'), text: () => t('cause.PROGRESS_TIMEOUT.text') },
  NO_ROUTE_DESTINATION: { cls: 'failed', label: () => t('cause.NO_ROUTE_DESTINATION.label'), text: (context) => t('cause.NO_ROUTE_DESTINATION.text', { context }) },
  UNALLOCATED_NUMBER: { cls: 'failed', label: () => t('cause.UNALLOCATED_NUMBER.label'), text: () => t('cause.UNALLOCATED_NUMBER.text') },
  USER_NOT_REGISTERED: { cls: 'failed', label: () => t('cause.USER_NOT_REGISTERED.label'), text: () => t('cause.USER_NOT_REGISTERED.text') },
  SUBSCRIBER_ABSENT: { cls: 'failed', label: () => t('cause.SUBSCRIBER_ABSENT.label'), text: () => t('cause.SUBSCRIBER_ABSENT.text') },
  NORMAL_TEMPORARY_FAILURE: { cls: 'failed', label: () => t('cause.NORMAL_TEMPORARY_FAILURE.label'), text: () => t('cause.NORMAL_TEMPORARY_FAILURE.text') },
  RECOVERY_ON_TIMER_EXPIRE: { cls: 'failed', label: () => t('cause.RECOVERY_ON_TIMER_EXPIRE.label'), text: () => t('cause.RECOVERY_ON_TIMER_EXPIRE.text') },
  INCOMPATIBLE_DESTINATION: { cls: 'failed', label: () => t('cause.INCOMPATIBLE_DESTINATION.label'), text: () => t('cause.INCOMPATIBLE_DESTINATION.text') },
  DESTINATION_OUT_OF_ORDER: { cls: 'failed', label: () => t('cause.DESTINATION_OUT_OF_ORDER.label'), text: () => t('cause.DESTINATION_OUT_OF_ORDER.text') },
  NETWORK_OUT_OF_ORDER: { cls: 'failed', label: () => t('cause.NETWORK_OUT_OF_ORDER.label'), text: () => t('cause.NETWORK_OUT_OF_ORDER.text') },
  MEDIA_TIMEOUT: { cls: 'failed', label: () => t('cause.MEDIA_TIMEOUT.label'), text: () => t('cause.MEDIA_TIMEOUT.text') },
  GATEWAY_DOWN: { cls: 'failed', label: () => t('cause.GATEWAY_DOWN.label'), text: () => t('cause.GATEWAY_DOWN.text') },
  INVALID_GATEWAY: { cls: 'failed', label: () => t('cause.INVALID_GATEWAY.label'), text: () => t('cause.INVALID_GATEWAY.text') },
  INVALID_NUMBER_FORMAT: { cls: 'failed', label: () => t('cause.INVALID_NUMBER_FORMAT.label'), text: () => t('cause.INVALID_NUMBER_FORMAT.text') },
  NORMAL_CIRCUIT_CONGESTION: { cls: 'failed', label: () => t('cause.NORMAL_CIRCUIT_CONGESTION.label'), text: () => t('cause.NORMAL_CIRCUIT_CONGESTION.text') },
  SWITCH_CONGESTION: { cls: 'failed', label: () => t('cause.SWITCH_CONGESTION.label'), text: () => t('cause.SWITCH_CONGESTION.text') },
  EXCHANGE_ROUTING_ERROR: { cls: 'failed', label: () => t('cause.EXCHANGE_ROUTING_ERROR.label'), text: () => t('cause.EXCHANGE_ROUTING_ERROR.text') },
  MANAGER_REQUEST: { cls: 'failed', label: () => t('cause.MANAGER_REQUEST.label'), text: () => t('cause.MANAGER_REQUEST.text') },
  SYSTEM_SHUTDOWN: { cls: 'failed', label: () => t('cause.SYSTEM_SHUTDOWN.label'), text: () => t('cause.SYSTEM_SHUTDOWN.text') },
  NORMAL_UNSPECIFIED: { cls: 'failed', label: () => t('cause.NORMAL_UNSPECIFIED.label'), text: () => t('cause.NORMAL_UNSPECIFIED.text') },
};

// "NO_ROUTE_DESTINATION" -> "No route destination"
function spelled(cause) {
  const s = cause.replace(/_/g, ' ').toLowerCase();
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// causeInfo returns {cls, label, text} for a hangup cause; context fills the
// {context} of the explanations that name one.
export function causeInfo(cause, context) {
  const name = String(cause || '');
  const c = Object.prototype.hasOwnProperty.call(CAUSES, name) ? CAUSES[name] : null;
  if (!c) return { cls: 'failed', label: t('cause.other.label', { cause: spelled(name) || '–' }), text: t('cause.other.text') };
  return { cls: c.cls, label: c.label(), text: c.text(context || '–') };
}

// The pill tone of each class.
export const CAUSE_TONE = { ok: 'ok', unanswered: 'warn', failed: 'bad' };
