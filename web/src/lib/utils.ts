import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

/** cn junta clases y resuelve las de Tailwind que se pisan entre sí. */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}
