import { onBeforeUnmount, onMounted, ref } from 'vue';

/** Number of open in-page modals; the shell navigation is made inert while any is open. */
export const openModalCount = ref(0);

export function useModalPresence() {
    onMounted(() => openModalCount.value++);
    onBeforeUnmount(() => openModalCount.value--);
}
