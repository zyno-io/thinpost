import { describe, expect, it } from 'vitest';

import { sortAppsByName } from '@/app-order';

describe('sortAppsByName', () => {
    it('orders by name ignoring case, with numbers in numeric order', () => {
        const apps = ['marketing', 'Billing', 'App 10', 'app 2', 'Zeta'].map(name => ({ name }));
        expect(sortAppsByName(apps).map(app => app.name)).toEqual(['app 2', 'App 10', 'Billing', 'marketing', 'Zeta']);
    });

    it('keeps the given order for equal names and does not change the input', () => {
        const apps = [{ name: 'b', id: 1 }, { name: 'A', id: 2 }, { name: 'a', id: 3 }];
        expect(sortAppsByName(apps).map(app => app.id)).toEqual([2, 3, 1]);
        expect(apps.map(app => app.id)).toEqual([1, 2, 3]);
    });
});
