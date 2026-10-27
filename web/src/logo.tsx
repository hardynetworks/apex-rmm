// Apex RMM logo mark: an apex "A" with a monitoring pulse for its crossbar.
import { useId } from 'preact/hooks';

export function Logo({ size = 28 }: { size?: number }) {
  const id = useId();
  return (
    <svg viewBox="0 0 32 32" width={size} height={size} role="img" aria-label="Apex RMM">
      <defs>
        <linearGradient id={id + 'g'} x1="0" y1="1" x2="1" y2="0">
          <stop offset="0" stop-color="#22d3ee" />
          <stop offset="0.5" stop-color="#3b82f6" />
          <stop offset="1" stop-color="#a855f7" />
        </linearGradient>
        <linearGradient id={id + 'b'} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stop-color="#16203a" />
          <stop offset="1" stop-color="#060a14" />
        </linearGradient>
      </defs>
      <rect width="32" height="32" rx="8" fill={`url(#${id}b)`} />
      <rect x="0.5" y="0.5" width="31" height="31" rx="7.5" fill="none" stroke={`url(#${id}g)`} stroke-opacity="0.6" />
      <path d="M7 25L16 7.5L25 25" fill="none" stroke={`url(#${id}g)`} stroke-width="3.2" stroke-linecap="round" stroke-linejoin="round" />
      <path d="M10.3 18.5H13L14.5 15.5L16.6 21.5L18.2 18.5H21.7" fill="none" stroke="#e0f2fe" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
      <circle cx="16" cy="7.5" r="3.6" fill="#22d3ee" opacity="0.3" />
      <circle cx="16" cy="7.5" r="1.9" fill="#fff" />
    </svg>
  );
}
