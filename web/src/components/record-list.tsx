'use client';

import { useState, useMemo, useRef, useEffect, memo } from 'react';
import { Record, getRecordColor, formatTimestamp, getRecordTitle } from '@/lib/types';
import { ScrollArea } from '@/components/ui/scroll-area';
import { Badge } from '@/components/ui/badge';
import { Input } from '@/components/ui/input';
import { cn } from '@/lib/utils';
import { 
  ArrowRight, 
  ArrowLeft, 
  Layers, 
  Zap, 
  Package, 
  AlertCircle,
  Circle
} from 'lucide-react';

// Get icon component for record type
function RecordIcon({ record }: { record: Record }) {
  const className = cn('w-4 h-4', getRecordColor(record));
  
  switch (record.type) {
    case 'request':
      return <ArrowRight className={className} />;
    case 'response':
      return <ArrowLeft className={className} />;
    case 'grpc':
      // Use arrow based on direction
      if (record.direction === 'C2S') {
        return <ArrowRight className={className} />;
      } else if (record.direction === 'S2C') {
        return <ArrowLeft className={className} />;
      }
      return <Layers className={className} />;
    case 'sse':
      return <Zap className={className} />;
    case 'body':
      // Use arrow based on direction for body too
      if (record.direction === 'C2S') {
        return <ArrowRight className={className} />;
      } else if (record.direction === 'S2C') {
        return <ArrowLeft className={className} />;
      }
      return <Package className={className} />;
    case 'error':
      return <AlertCircle className={className} />;
    default:
      return <Circle className={className} />;
  }
}

interface RecordListProps {
  records: Record[];
  selectedRecord: Record | null;
  onSelectRecord: (record: Record) => void;
}

export const RecordList = memo(function RecordList({
  records,
  selectedRecord,
  onSelectRecord,
}: RecordListProps) {
  const [localSearch, setLocalSearch] = useState('');
  const [matchIndex, setMatchIndex] = useState(0);
  const scrollRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<Map<string, HTMLButtonElement>>(new Map());

  // Filter records by local search
  const { filteredRecords, matchedIndices } = useMemo(() => {
    if (!localSearch.trim()) {
      return { filteredRecords: records, matchedIndices: [] };
    }
    
    const query = localSearch.toLowerCase();
    const matched: number[] = [];
    
    records.forEach((r, idx) => {
      const searchText = [
        r.url,
        r.grpc_service,
        r.grpc_method,
        r.grpc_data,
        r.body,
        r.host,
        r.error,
      ].filter(Boolean).join(' ').toLowerCase();
      
      if (searchText.includes(query)) {
        matched.push(idx);
      }
    });
    
    return { filteredRecords: records, matchedIndices: matched };
  }, [records, localSearch]);

  // Scroll to matched item
  useEffect(() => {
    if (matchedIndices.length > 0 && localSearch) {
      const targetIdx = matchedIndices[matchIndex % matchedIndices.length];
      const record = records[targetIdx];
      const key = `${record.session}-${record.index}`;
      const element = itemRefs.current.get(key);
      if (element) {
        element.scrollIntoView({ behavior: 'smooth', block: 'center' });
        onSelectRecord(record);
      }
    }
  }, [matchIndex, matchedIndices, localSearch, records, onSelectRecord]);

  // Handle search input key events
  const handleSearchKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      if (matchedIndices.length > 0) {
        setMatchIndex((prev) => (prev + 1) % matchedIndices.length);
      }
    }
  };

  return (
    <div className="flex flex-col h-full overflow-hidden border-r">
      {/* Search bar */}
      <div className="p-2 border-b flex-shrink-0">
        <div className="relative">
          <Input
            placeholder="Search records... (Enter to next)"
            value={localSearch}
            onChange={(e) => {
              setLocalSearch(e.target.value);
              setMatchIndex(0);
            }}
            onKeyDown={handleSearchKeyDown}
            className="h-8 text-xs pr-16"
          />
          {localSearch && matchedIndices.length > 0 && (
            <span className="absolute right-2 top-1/2 -translate-y-1/2 text-xs text-gray-600 dark:text-gray-400">
              {matchIndex + 1}/{matchedIndices.length}
            </span>
          )}
        </div>
      </div>

      <ScrollArea className="flex-1 min-h-0" ref={scrollRef}>
        <div className="divide-y">
          {filteredRecords.map((record, idx) => {
            const isSelected =
              selectedRecord?.session === record.session &&
              selectedRecord?.index === record.index;
            const isMatched = localSearch && matchedIndices.includes(idx);
            const isCurrentMatch = isMatched && matchedIndices[matchIndex % matchedIndices.length] === idx;
            const key = `${record.session}-${record.index}`;

            return (
              <button
                key={`${key}-${idx}`}
                ref={(el) => {
                  if (el) itemRefs.current.set(key, el);
                }}
                onClick={() => onSelectRecord(record)}
                className={cn(
                  'w-full text-left px-3 py-2 transition-colors',
                  isSelected ? 'bg-blue-50 dark:bg-blue-900/30' : 'hover:bg-gray-50 dark:hover:bg-gray-800/50',
                  isCurrentMatch && 'ring-2 ring-yellow-400 ring-inset',
                  isMatched && !isCurrentMatch && 'bg-yellow-50 dark:bg-yellow-900/20'
                )}
              >
                <div className="flex items-center gap-2">
                  <RecordIcon record={record} />
                  <span className="text-xs text-gray-500 dark:text-gray-500 font-mono">
                    #{record.index}
                  </span>
                  <Badge variant="outline" className="text-xs">
                    {record.type}
                  </Badge>
                  {record.grpc_streaming && (
                    <Badge variant="secondary" className="text-xs">
                      stream
                    </Badge>
                  )}
                </div>
                <div className="mt-1 text-sm truncate">
                  {getRecordTitle(record)}
                </div>
                <div className="mt-1 text-xs text-gray-600 dark:text-gray-400 flex items-center gap-2">
                  <span>{formatTimestamp(record.ts)}</span>
                  {record.size !== undefined && record.size > 0 && (
                    <span>· {record.size}B</span>
                  )}
                  {record.grpc_frame_index !== undefined && (
                    <span>· frame {record.grpc_frame_index}</span>
                  )}
                </div>
              </button>
            );
          })}

          {filteredRecords.length === 0 && (
            <div className="text-center text-gray-500 dark:text-gray-400 text-sm py-8">
              No records
            </div>
          )}
        </div>
      </ScrollArea>
    </div>
  );
}, (prevProps, nextProps) => {
  // Custom comparison - re-render only when these change
  const recordsEqual = prevProps.records.length === nextProps.records.length &&
    (prevProps.records.length === 0 || 
      (prevProps.records[prevProps.records.length - 1]?.session === nextProps.records[nextProps.records.length - 1]?.session &&
       prevProps.records[prevProps.records.length - 1]?.index === nextProps.records[nextProps.records.length - 1]?.index));
  
  const selectedEqual = 
    prevProps.selectedRecord?.session === nextProps.selectedRecord?.session &&
    prevProps.selectedRecord?.index === nextProps.selectedRecord?.index;
  
  return recordsEqual && selectedEqual && prevProps.onSelectRecord === nextProps.onSelectRecord;
});
