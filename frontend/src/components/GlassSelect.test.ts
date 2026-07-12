import { mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import GlassSelect from '@/components/GlassSelect.vue'

const options = [
  { label: 'Alpha', value: 'alpha' },
  { label: 'Beta', value: 'beta' },
  { label: 'Gamma', value: 'gamma' },
]

function lastModelUpdate(wrapper: ReturnType<typeof mount>) {
  const events = wrapper.emitted('update:modelValue') || []
  return events[events.length - 1]
}

describe('GlassSelect', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('closes and restores focus after a mouse choice, then can reopen immediately', async () => {
    const wrapper = mount(GlassSelect, {
      attachTo: document.body,
      props: { modelValue: 'alpha', options, ariaLabel: '测试选择器' },
    })
    const trigger = wrapper.get('[role="combobox"]')

    await trigger.trigger('click')
    expect(trigger.attributes('aria-expanded')).toBe('true')
    await wrapper.findAll('[role="option"]')[1].trigger('click')
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[role="listbox"]').exists()).toBe(false)
    expect(trigger.attributes('aria-expanded')).toBe('false')
    expect(document.activeElement).toBe(trigger.element)
    expect(lastModelUpdate(wrapper)).toEqual(['beta'])

    await vi.runOnlyPendingTimersAsync()
    await trigger.trigger('click')
    expect(trigger.attributes('aria-expanded')).toBe('true')
    wrapper.unmount()
  })

  it('supports Arrow navigation, Enter selection and Escape focus restoration', async () => {
    const wrapper = mount(GlassSelect, {
      attachTo: document.body,
      props: { modelValue: 'alpha', options },
    })
    const trigger = wrapper.get('[role="combobox"]')

    await trigger.trigger('keydown', { key: 'ArrowDown' })
    await wrapper.vm.$nextTick()
    await trigger.trigger('keydown', { key: 'ArrowDown' })
    await trigger.trigger('keydown', { key: 'Enter' })
    expect(lastModelUpdate(wrapper)).toEqual(['beta'])
    expect(trigger.attributes('aria-expanded')).toBe('false')

    await trigger.trigger('keydown', { key: 'ArrowDown' })
    await wrapper.vm.$nextTick()
    await trigger.trigger('keydown', { key: 'Escape' })
    await wrapper.vm.$nextTick()
    expect(trigger.attributes('aria-expanded')).toBe('false')
    expect(document.activeElement).toBe(trigger.element)
    wrapper.unmount()
  })

  it('supports typeahead while the listbox is open', async () => {
    const wrapper = mount(GlassSelect, {
      props: { modelValue: 'alpha', options },
    })
    const trigger = wrapper.get('[role="combobox"]')
    await trigger.trigger('click')
    await wrapper.vm.$nextTick()
    await trigger.trigger('keydown', { key: 'g' })
    await wrapper.vm.$nextTick()

    expect(trigger.attributes('aria-activedescendant')).toContain('option-2')
    await trigger.trigger('keydown', { key: 'Enter' })
    expect(lastModelUpdate(wrapper)).toEqual(['gamma'])
    wrapper.unmount()
  })
})
