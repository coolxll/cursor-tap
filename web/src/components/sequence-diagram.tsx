'use client';

import { useEffect, useId, useState } from 'react';

interface SequenceDiagramProps {
  chart: string;
}

export function SequenceDiagram({ chart }: SequenceDiagramProps) {
  const id = useId().replace(/:/g, '');
  const [svg, setSvg] = useState('');
  const [error, setError] = useState('');

  useEffect(() => {
    let cancelled = false;
    async function render() {
      try {
        const mermaid = (await import('mermaid')).default;
        mermaid.initialize({
          startOnLoad: false,
          securityLevel: 'strict',
          theme: 'base',
          sequence: {
            showSequenceNumbers: true,
            mirrorActors: false,
          },
        });
        const result = await mermaid.render(`sequence-${id}`, chart);
        if (!cancelled) {
          setSvg(result.svg);
          setError('');
        }
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err));
          setSvg('');
        }
      }
    }
    render();
    return () => {
      cancelled = true;
    };
  }, [chart, id]);

  if (error) {
    return <div className="rounded-md border border-danger/30 bg-danger/10 p-3 text-xs text-danger">{error}</div>;
  }

  return (
    <div className="min-h-48 overflow-auto rounded-md border border-divider bg-content1 p-3">
      {svg ? (
        <div className="min-w-[680px]" dangerouslySetInnerHTML={{ __html: svg }} />
      ) : (
        <div className="text-sm text-default-500">Rendering timeline...</div>
      )}
    </div>
  );
}
