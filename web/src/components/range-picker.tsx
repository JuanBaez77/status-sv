import { Button } from '@/components/ui/button'
import { ranges, type RangeKey } from '@/lib/ranges'

/** RangePicker elige cuánto tiempo hacia atrás muestran los gráficos. */
export function RangePicker({
  value,
  onChange,
}: {
  value: RangeKey
  onChange: (range: RangeKey) => void
}) {
  return (
    <div role="group" aria-label="Rango de tiempo" className="flex flex-wrap gap-1">
      {ranges.map((range) => (
        <Button
          key={range.key}
          size="sm"
          variant={range.key === value ? 'secondary' : 'ghost'}
          aria-pressed={range.key === value}
          onClick={() => onChange(range.key)}
        >
          {range.label}
        </Button>
      ))}
    </div>
  )
}
